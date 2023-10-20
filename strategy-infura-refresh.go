package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/elbv2"
)

func strategyInfuraRefresh(ctx context.Context, region string, asgName string) error {
	// add a timeout
	ctx, cancel := context.WithTimeout(ctx, time.Hour)
	defer cancel()

	sess := session.Must(session.NewSession(aws.NewConfig().WithRegion(region)))

	// Record rollbacks during the procedure to get back on a reasonable state if the context get cancelled
	var rollbacks []func(ctx context.Context) error

	start := time.Now()

	originalAsg, err := getASGDetails(ctx, sess, asgName)
	if err != nil {
		return err
	}

	log.Printf("Refresh started (asg: %s, region: %s, strategy: infura-refresh)", asgName, region)
	log.Printf("ASG protect instances by default: %v", originalAsg.newInstanceProtected)
	log.Printf("Existing instances:")
	for _, inst := range originalAsg.instances {
		log.Printf("\t%s", inst)
	}

	log.Printf("Disable autoscaling processes")
	{
		rollbacks = append(rollbacks, func(ctx context.Context) error {
			log.Printf("Restore autoscaling processes")
			return resumeAutoscaling(ctx, sess, asgName, originalAsg.suspendedProcesses)
		})

		err = suspendAutoscaling(ctx, sess, asgName)
		if err != nil {
			rollback(err, rollbacks)
			return err
		}
	}

	// Set instance protection on the old instances to be sure we keep having them around in case of failure
	log.Printf("Enable instance protection on old instances")
	{
		if !originalAsg.newInstanceProtected {
			rollbacks = append(rollbacks, func(ctx context.Context) error {
				log.Printf("Removing instance protection on old instances")
				return removeInstanceProtection(ctx, sess, asgName, originalAsg.instances)
			})
		}

		err = setInstanceProtection(ctx, sess, asgName, originalAsg.instances)
		if err != nil {
			rollback(err, rollbacks)
			return err
		}
	}

	// Double the ASG sizes
	log.Printf("Double the ASG size:")
	log.Printf("\tmaxSize %d-->%d", originalAsg.maxSize, 2*originalAsg.maxSize)
	log.Printf("\tdesiredCapacity %d-->%d", originalAsg.desiredCapacity, 2*originalAsg.desiredCapacity)
	{
		rollbacks = append(rollbacks, func(ctx context.Context) error {
			// Simple wait instead of monitoring the instances because we are already in a broken situation.
			log.Printf("Wait 10 min for the ASG to stabilize")
			time.Sleep(10 * time.Minute)
			return nil
		})
		rollbacks = append(rollbacks, func(ctx context.Context) error {
			log.Printf("Rolling back ASG settings")
			return updateASG(ctx, sess, asgName, originalAsg.maxSize, originalAsg.desiredCapacity, originalAsg.newInstanceProtected)
		})

		// No instance protection on the new instances so that the ASG can replace a failed instance if necessary
		err = updateASG(ctx, sess, asgName, 2*originalAsg.maxSize, 2*originalAsg.desiredCapacity, false)
		if err != nil {
			rollback(err, rollbacks)
			return err
		}
	}

	log.Printf("Detect new instances and wait for them to be healthy")
	newInstances, err := waitForNewInstances(ctx, sess, asgName, int(originalAsg.desiredCapacity), originalAsg.targetGroups, originalAsg.instances)
	if err != nil {
		rollbacks = append(rollbacks, func(ctx context.Context) error {
			if len(newInstances) > 0 {
				return removeInstanceProtection(ctx, sess, asgName, newInstances)
			}
			return nil
		})
		rollback(err, rollbacks)
		return err
	}

	// unprotect the old instances
	log.Printf("Remove instance protection on the old instances")
	err = removeInstanceProtection(ctx, sess, asgName, originalAsg.instances)
	if err != nil {
		rollback(err, rollbacks)
		return err
	}

	// get back to normal size, cull the old instances
	log.Printf("Set back the ASG size:")
	log.Printf("\tmaxSize %d-->%d", 2*originalAsg.maxSize, originalAsg.maxSize)
	log.Printf("\tdesiredCapacity %d-->%d", 2*originalAsg.desiredCapacity, originalAsg.desiredCapacity)
	err = updateASG(ctx, sess, asgName, originalAsg.maxSize, originalAsg.desiredCapacity, originalAsg.newInstanceProtected)
	if err != nil {
		rollback(err, rollbacks)
		return err
	}

	// At this point:
	// - the ASG is back to normal
	// - old instances have the protection disabled
	// - new instances have the protection enabled
	// For the rollback:
	// - wait 10 minutes to give the ASG a chance to stabilize with keeping the new instances and removing the old ones
	// - make sure to have the protection set as desired on the new instances
	log.Printf("Wait for scaling in")
	{
		rollbacks = []func(ctx context.Context) error{
			func(ctx context.Context) error {
				log.Printf("Wait 10 min for the ASG to stabilize")
				time.Sleep(10 * time.Minute)
				if !originalAsg.newInstanceProtected {
					log.Printf("Remove instance protection on the new instances")
					return removeInstanceProtection(ctx, sess, asgName, newInstances)
				}
				return nil
			},
		}

		err = waitForInstanceCount(ctx, sess, asgName, int(originalAsg.desiredCapacity))
		if err != nil {
			rollback(err, rollbacks)
			return err
		}
	}

	// unprotect the new instances if needed
	if !originalAsg.newInstanceProtected {
		log.Printf("Remove instance protection on the new instances")
		err = removeInstanceProtection(ctx, sess, asgName, newInstances)
		if err != nil {
			return err
		}
	}

	log.Printf("Restore autoscaling processes")
	{
		err = resumeAutoscaling(ctx, sess, asgName, originalAsg.suspendedProcesses)
		if err != nil {
			return err
		}
	}

	log.Printf("Done in %s.", time.Since(start))

	return nil
}

// rollback execute the given rollback steps in *reverse order*
func rollback(err error, steps []func(ctx context.Context) error) {
	rollbackCtx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	log.Printf("======================================================")
	log.Printf("ROLLBACK due to error: %v", err)
	log.Printf("======================================================")

	// rolling back in opposite order
	for i := len(steps) - 1; i >= 0; i-- {
		err := steps[i](rollbackCtx)
		if err != nil {
			log.Printf("-> Error during rollback: %v", err)
			log.Printf("-> Attempting to continue rollback")
		}
	}
}

// asg hold the details of and AWS autoscaling group
type asg struct {
	// the known instances
	instances []instance
	// the first target group defined to attach the instance to, if any
	targetGroups    []targetGroup
	maxSize         int64
	desiredCapacity int64
	// whether or not the new instances get automatically protected against scale-in termination
	newInstanceProtected bool
	// the suspended autoscaling process, if any
	suspendedProcesses map[string]struct{}
}

type instance struct {
	instanceId     string
	lifecycleState string
	healthStatus   string
}

func (i instance) String() string {
	return fmt.Sprintf("%s lifecycle:%s asg-based-health:%s", i.instanceId, i.lifecycleState, i.healthStatus)
}

type targetGroup struct {
	arn string
}

// getASGDetails retrieve the relevant details from an AWS autoscaling group
func getASGDetails(ctx context.Context, sess *session.Session, asgName string) (*asg, error) {
	as := autoscaling.New(sess)

	out, err := as.DescribeAutoScalingGroupsWithContext(ctx, &autoscaling.DescribeAutoScalingGroupsInput{
		AutoScalingGroupNames: aws.StringSlice([]string{asgName}),
	})
	if err != nil {
		return nil, err
	}

	if len(out.AutoScalingGroups) != 1 {
		return nil, fmt.Errorf("getInstancesAndTG: unexpected asg count of %d", len(out.AutoScalingGroups))
	}

	instances := make([]instance, len(out.AutoScalingGroups[0].Instances))

	for i, inst := range out.AutoScalingGroups[0].Instances {
		instances[i] = instance{
			instanceId:     *inst.InstanceId,
			lifecycleState: *inst.LifecycleState,
			healthStatus:   *inst.HealthStatus,
		}
	}

	var targetGroups []targetGroup
	for _, tgs := range out.AutoScalingGroups[0].TargetGroupARNs {
		targetGroups = append(targetGroups, targetGroup{
			arn: *tgs,
		})
	}

	suspendedProcesses := make(map[string]struct{}, len(out.AutoScalingGroups[0].SuspendedProcesses))

	for _, process := range out.AutoScalingGroups[0].SuspendedProcesses {
		suspendedProcesses[*process.ProcessName] = struct{}{}
	}

	return &asg{
		instances:            instances,
		targetGroups:         targetGroups,
		maxSize:              *out.AutoScalingGroups[0].MaxSize,
		desiredCapacity:      *out.AutoScalingGroups[0].DesiredCapacity,
		newInstanceProtected: *out.AutoScalingGroups[0].NewInstancesProtectedFromScaleIn,
		suspendedProcesses:   suspendedProcesses,
	}, nil
}

// updateASG set a few properties of an AWS autoscaling group, relevant for controlling a deployment
func updateASG(ctx context.Context, sess *session.Session, asgName string, maxSize, desiredCapacity int64, enableProtection bool) error {
	as := autoscaling.New(sess)

	_, err := as.UpdateAutoScalingGroupWithContext(ctx, &autoscaling.UpdateAutoScalingGroupInput{
		AutoScalingGroupName:             aws.String(asgName),
		DesiredCapacity:                  aws.Int64(desiredCapacity),
		MaxSize:                          aws.Int64(maxSize),
		NewInstancesProtectedFromScaleIn: aws.Bool(enableProtection),
	})
	return err
}

// setInstanceProtection enable the scale-in protection on the given instances, to prevent the ASG from removing the instance
func setInstanceProtection(ctx context.Context, sess *session.Session, asgName string, instances []instance) error {
	as := autoscaling.New(sess)

	ids := make([]*string, len(instances))
	for i, inst := range instances {
		id := inst.instanceId
		ids[i] = &id
	}

	_, err := as.SetInstanceProtectionWithContext(ctx, &autoscaling.SetInstanceProtectionInput{
		AutoScalingGroupName: aws.String(asgName),
		InstanceIds:          ids,
		ProtectedFromScaleIn: aws.Bool(true),
	})

	return err
}

// removeInstanceProtection remove the scale-in protection on the given instances, to give back the control to the ASG
func removeInstanceProtection(ctx context.Context, sess *session.Session, asgName string, instances []instance) error {
	as := autoscaling.New(sess)

	ids := make([]*string, len(instances))
	for i, inst := range instances {
		id := inst.instanceId
		ids[i] = &id
	}

	_, err := as.SetInstanceProtectionWithContext(ctx, &autoscaling.SetInstanceProtectionInput{
		AutoScalingGroupName: aws.String(asgName),
		InstanceIds:          ids,
		ProtectedFromScaleIn: aws.Bool(false),
	})

	return err
}

// suspendAutoscaling disable the autonomous scaling actions on an ASG
// - scaling based on cloudwatch alarms
// - scaling based on predictive features
func suspendAutoscaling(ctx context.Context, sess *session.Session, asgName string) error {
	as := autoscaling.New(sess)

	_, err := as.SuspendProcessesWithContext(ctx, &autoscaling.ScalingProcessQuery{
		AutoScalingGroupName: aws.String(asgName),
		ScalingProcesses: []*string{
			aws.String("AlarmNotification"),
			aws.String("ScheduledActions"),
			aws.String("AZRebalance"),
		},
	})

	return err
}

// resumeAutoscaling enable the given autonomous scaling actions on an ASG
func resumeAutoscaling(ctx context.Context, sess *session.Session, asgName string, originalSuspendedProcesses map[string]struct{}) error {
	as := autoscaling.New(sess)

	// we need to construct the list of process we want to resume

	var toResume []*string

	if _, ok := originalSuspendedProcesses["AlarmNotification"]; !ok {
		toResume = append(toResume, aws.String("AlarmNotification"))
	}
	if _, ok := originalSuspendedProcesses["ScheduledActions"]; !ok {
		toResume = append(toResume, aws.String("ScheduledActions"))
	}
	if _, ok := originalSuspendedProcesses["AZRebalance"]; !ok {
		toResume = append(toResume, aws.String("AZRebalance"))
	}

	_, err := as.ResumeProcessesWithContext(ctx, &autoscaling.ScalingProcessQuery{
		AutoScalingGroupName: aws.String(asgName),
		ScalingProcesses:     toResume,
	})

	return err
}

// waitForNewInstances:
// - poll the ASG to detect new instances
// - wait for them to be ready at the ASG level
// - if any, wait for them to be ready at the TG level
// - when ready, immediately enable scale-in protection to prevent them being destroyed
//
// Returns when count instance has been found.
func waitForNewInstances(ctx context.Context, sess *session.Session, asgName string, count int, tgs []targetGroup, currentInstances []instance) ([]instance, error) {
	// there is **four** endless loop bound to the the context lifecycle here:
	// - one in `detectNewInstances()` doing ASG polling, which output to two **channels** for 1) values and 2) errors
	// - the goroutine in this function that log and discards the errors from the error channel
	// - the value channel get passed to `detectInstancesReady()` which poll the ASG health check and possibly the TG.
	//   This function also output to two channels for values and errors
	// - the for loop here that 1) log and discard errors from the second error channel and 2) read from the value channel,
	//   set the instance protection and accumulate the healthy instances until we have the expected count

	newInstances, chanErr := detectNewInstances(ctx, sess, asgName, currentInstances)

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case err := <-chanErr:
				if err != nil {
					log.Printf("\terror while polling ASG details: %v", err)
				}
				continue
			}
		}
	}()

	// make sure to stop the following call when done
	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	readyInstances, chanErr2 := detectInstancesReady(subCtx, sess, tgs, newInstances)

	instanceReady := make([]instance, 0, count)

	for {
		select {
		case <-ctx.Done():
			return instanceReady, ctx.Err()
		case err := <-chanErr2:
			if err != nil {
				log.Printf("\terror while polling autoscaling or target group reported target health: %v", err)
			}
			continue
		case inst, ok := <-readyInstances:
			if !ok {
				// channel is closed, time to pack our things
				return nil, nil
			}
			log.Printf("\t(%d/%d) Instance %s is ready, enabling scale-in protection ...", len(instanceReady)+1, count, inst.instanceId)

			err := setInstanceProtection(ctx, sess, asgName, []instance{inst})
			if err != nil {
				return instanceReady, err
			}
			log.Printf("\tScale-in protection enabled on %s", inst.instanceId)

			instanceReady = append(instanceReady, inst)

			if len(instanceReady) >= count {
				return instanceReady, nil
			}
		}
	}
}

// detectNewInstances poll the ASG to detect new instances. Instances given in currentInstances will not be reported.
func detectNewInstances(ctx context.Context, sess *session.Session, asgName string, currentInstances []instance) (chan instance, chan error) {
	knownSet := make(map[string]instance)
	for _, inst := range currentInstances {
		knownSet[inst.instanceId] = inst
	}

	chanOut := make(chan instance)
	chanErr := make(chan error)

	period := 20 * time.Second

	go func() {
		defer close(chanOut)
		defer close(chanErr)

		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(period):
			}

			asg, err := getASGDetails(ctx, sess, asgName)
			if err != nil {
				chanErr <- err
				// don't stop on error, let the caller decide what to do
				continue
			}

			for _, inst := range asg.instances {
				if _, has := knownSet[inst.instanceId]; !has {
					chanOut <- inst
					knownSet[inst.instanceId] = inst
				}
			}
		}
	}()

	return chanOut, chanErr
}

// detectInstancesReady poll the ASG to detect when instances passed via the newInstances channel are considered healthy.
// If a target group is also given, it will also be polled for instance healthiness, which will ultimately query the instance custom health-check.
func detectInstancesReady(ctx context.Context, sess *session.Session, tgs []targetGroup, newInstances chan instance) (chan instance, chan error) {
	chanOut := make(chan instance)
	chanErr := make(chan error)

	period := 2 * time.Minute

	go func() {
		defer close(chanOut)
		defer close(chanErr)

		instanceSet := make(map[string]instance)

		for {
			select {
			case <-ctx.Done():
				return
			case inst, ok := <-newInstances:
				if !ok {
					// channel is closed, time to pack our things
					return
				}
				log.Printf("\tfound new instance: %s", inst)
				instanceSet[inst.instanceId] = inst
			case <-time.After(period):
				// wait to have at least one instance to inspect
				if len(instanceSet) == 0 {
					continue
				}
			}

			// inspect the instance health
			healthyInstances, err := getHealthyAutoscalingInstances(ctx, sess, instanceSet)
			if err != nil {
				chanErr <- err
				// don't stop on error, let the caller decide what to do
				continue
			}

			// if we don't have a target group, end the checks here
			if len(tgs) == 0 {
				for _, inst := range healthyInstances {
					delete(instanceSet, inst.instanceId)
					chanOut <- inst
				}
				continue
			}

			if len(healthyInstances) == 0 {
				// wait to have at least some instance ASG ready
				continue
			}

			// we have a target group, let's also inspect the reported health
			healthyInstances, err = getHealthyTGInstances(ctx, sess, tgs, healthyInstances)
			if err != nil {
				chanErr <- err
				// don't stop on error, let the caller decide what to do
				continue
			}

			for _, inst := range healthyInstances {
				delete(instanceSet, inst.instanceId)
				chanOut <- inst
			}
		}
	}()

	return chanOut, chanErr
}

// getHealthyAutoscalingInstances query an ASG and report which of the given instances are healthy.
func getHealthyAutoscalingInstances(ctx context.Context, sess *session.Session, instances map[string]instance) ([]instance, error) {
	as := autoscaling.New(sess)

	instanceIds := make([]*string, 0, len(instances))
	for id := range instances {
		instanceIds = append(instanceIds, aws.String(id))
	}

	out, err := as.DescribeAutoScalingInstancesWithContext(ctx, &autoscaling.DescribeAutoScalingInstancesInput{
		InstanceIds: instanceIds,
	})
	if err != nil {
		return nil, err
	}

	var healthyInstances []instance

	for _, details := range out.AutoScalingInstances {
		if strings.ToLower(*details.HealthStatus) == "healthy" && *details.LifecycleState == autoscaling.LifecycleStateInService {
			healthyInstances = append(healthyInstances, instance{
				instanceId:     *details.InstanceId,
				lifecycleState: *details.LifecycleState,
				healthStatus:   *details.HealthStatus,
			})
		}
	}

	return healthyInstances, nil
}

// getHealthyTGInstances query all TGs and report which of the given instances are healthy in all of them.
func getHealthyTGInstances(ctx context.Context, sess *session.Session, tgs []targetGroup, instances []instance) ([]instance, error) {
	elb := elbv2.New(sess)

	instanceMap := make(map[string]instance)
	targets := make([]*elbv2.TargetDescription, 0, len(instances))

	for _, inst := range instances {
		instanceMap[inst.instanceId] = inst
		targets = append(targets, &elbv2.TargetDescription{
			Id: aws.String(inst.instanceId),
		})
	}

	healthyInstancesSet := make(map[string]int)

	for _, tg := range tgs {
		out, err := elb.DescribeTargetHealthWithContext(ctx, &elbv2.DescribeTargetHealthInput{
			TargetGroupArn: aws.String(tg.arn),
			Targets:        targets,
		})
		if err != nil {
			return nil, err
		}

		for _, description := range out.TargetHealthDescriptions {
			if *description.TargetHealth.State == elbv2.TargetHealthStateEnumHealthy {
				_, ok := healthyInstancesSet[*description.Target.Id]
				if ok {
					healthyInstancesSet[*description.Target.Id]++
				} else {
					healthyInstancesSet[*description.Target.Id] = 1
				}
			}
		}
	}

	var healthyInstances []instance

	for id, count := range healthyInstancesSet {
		if count == len(tgs) {
			healthyInstances = append(healthyInstances, instanceMap[id])
		}
	}

	return healthyInstances, nil
}

// waitForInstanceCount poll an ASG until the instance count is *lower or equal* the given count.
func waitForInstanceCount(ctx context.Context, sess *session.Session, asgName string, count int) error {
	period := 2 * time.Minute

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(period):
		}

		asg, err := getASGDetails(ctx, sess, asgName)
		if err != nil {
			return err
		}

		log.Printf("\tInstance count: %d", len(asg.instances))
		if len(asg.instances) <= count {
			return nil
		}
	}
}
