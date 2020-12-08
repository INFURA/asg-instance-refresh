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
	newInstances, err := waitForNewInstances(ctx, sess, asgName, int(originalAsg.desiredCapacity), originalAsg.tg, originalAsg.instances)
	if err != nil {
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
	// - make sure to remove have the protection set as desired on the new instances
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

	log.Printf("Done in %s.", time.Since(start))

	return nil
}

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

type asg struct {
	instances            []instance
	tg                   *targetGroup
	maxSize              int64
	desiredCapacity      int64
	newInstanceProtected bool
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

	var tg *targetGroup

	if len(out.AutoScalingGroups[0].TargetGroupARNs) > 0 {
		tg = &targetGroup{
			arn: *out.AutoScalingGroups[0].TargetGroupARNs[0],
		}
	}

	return &asg{
		instances:            instances,
		tg:                   tg,
		maxSize:              *out.AutoScalingGroups[0].MaxSize,
		desiredCapacity:      *out.AutoScalingGroups[0].DesiredCapacity,
		newInstanceProtected: *out.AutoScalingGroups[0].NewInstancesProtectedFromScaleIn,
	}, nil
}

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

// waitForNewInstances:
// - poll the ASG to detect new instances
// - wait for them to be ready at the ASG level
// - if any, wait for them to be ready at the TG level
// - when ready, immediately enable scale-in protection to prevent them being destroyed
//
// Returns when count instance has been found.
func waitForNewInstances(ctx context.Context, sess *session.Session, asgName string, count int, tg *targetGroup, currentInstances []instance) ([]instance, error) {
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

	readyInstances, chanErr2 := detectInstancesReady(subCtx, sess, tg, newInstances)

	instanceReady := make([]instance, 0, count)

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case err := <-chanErr2:
			if err != nil {
				log.Printf("\terror while polling autoscaling or target group reported target health: %v", err)
			}
			continue
		case inst := <-readyInstances:
			instanceReady = append(instanceReady, inst)
			log.Printf("\t(%d/%d) Instance %s is ready, enabling scale-in protection ...", len(instanceReady), count, inst.instanceId)

			err := setInstanceProtection(ctx, sess, asgName, []instance{inst})
			if err != nil {
				return nil, err
			}
			log.Printf("\tScale-in protection enabled on %s", inst.instanceId)

			if len(instanceReady) >= count {
				return instanceReady, nil
			}
		}
	}
}

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

func detectInstancesReady(ctx context.Context, sess *session.Session, tg *targetGroup, newInstances chan instance) (chan instance, chan error) {
	chanOut := make(chan instance)
	chanErr := make(chan error)

	period := 20 * time.Second

	go func() {
		defer close(chanOut)
		defer close(chanErr)

		instanceSet := make(map[string]instance)

		for {
			select {
			case <-ctx.Done():
				return
			case inst := <-newInstances:
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
			if tg == nil {
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
			healthyInstances, err = getHealthyTGInstances(ctx, sess, *tg, healthyInstances)
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

func getHealthyTGInstances(ctx context.Context, sess *session.Session, tg targetGroup, instances []instance) ([]instance, error) {
	elb := elbv2.New(sess)

	instanceMap := make(map[string]instance)
	targets := make([]*elbv2.TargetDescription, 0, len(instances))

	for _, inst := range instances {
		instanceMap[inst.instanceId] = inst
		targets = append(targets, &elbv2.TargetDescription{
			Id: aws.String(inst.instanceId),
		})
	}

	out, err := elb.DescribeTargetHealthWithContext(ctx, &elbv2.DescribeTargetHealthInput{
		TargetGroupArn: aws.String(tg.arn),
		Targets:        targets,
	})
	if err != nil {
		return nil, err
	}

	var healthyInstances []instance

	for _, description := range out.TargetHealthDescriptions {
		if *description.TargetHealth.State == elbv2.TargetHealthStateEnumHealthy {
			healthyInstances = append(healthyInstances, instanceMap[*description.Target.Id])
		}
	}

	return healthyInstances, nil
}

func waitForInstanceCount(ctx context.Context, sess *session.Session, asgName string, count int) error {
	period := 20 * time.Second

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
