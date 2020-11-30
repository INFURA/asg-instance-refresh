package main

import (
	"context"
	"fmt"
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

	start := time.Now()

	originalAsg, err := getASGDetails(ctx, sess, asgName)
	if err != nil {
		return err
	}

	fmt.Printf("Refresh (strategy: infura-refresh) started on ASG %s\n", asgName)
	fmt.Printf("Existing instances:\n")
	for _, inst := range originalAsg.instances {
		fmt.Printf("\t%s\n", inst)
	}

	// Record rollbacks during the procedure to get back on a reasonable state if the context get cancelled
	// var rollbacks []func(ctx context.Context) error
	//
	// rollbacks = append(rollbacks, func(ctx context.Context) error {
	// 	return setASGCapacities(ctx, as, asgName, originalAsg.maxSize, originalAsg.desiredCapacity)
	// })

	// Set instance protection on the old instances to be sure we keep having them around in case of failure
	fmt.Printf("Enable instance protection on old instances\n")
	err = setInstanceProtection(ctx, sess, asgName, originalAsg.instances)
	if err != nil {
		return err
	}

	// Double the ASG sizes
	fmt.Printf("Double the ASG size:\n\tmaxSize %d-->%d\n\tdesiredCapacity %d-->%d\n",
		originalAsg.maxSize, 2*originalAsg.maxSize, originalAsg.desiredCapacity, 2*originalAsg.desiredCapacity)
	err = setASGCapacities(ctx, sess, asgName, 2*originalAsg.maxSize, 2*originalAsg.desiredCapacity)
	if err != nil {
		return err
	}

	fmt.Printf("Detect new instances and wait for them to be healthy\n")
	newInstances, err := waitForNewInstances(ctx, sess, asgName, int(originalAsg.desiredCapacity), originalAsg.tg, originalAsg.instances)
	if err != nil {
		// rollback !
		// TODO
	}

	// protect the new instances
	fmt.Printf("Enable instance protection on the new instances\n")
	err = setInstanceProtection(ctx, sess, asgName, newInstances)
	if err != nil {
		// rollback !
		// TODO
	}

	// unprotect the old instances
	fmt.Printf("Remove instance protection on the old instances\n")
	err = removeInstanceProtection(ctx, sess, asgName, originalAsg.instances)
	if err != nil {
		// rollback !
		// TODO
	}

	// get back to normal size, cull the old instances
	fmt.Printf("Set back the ASG size:\n\tmaxSize %d-->%d\n\tdesiredCapacity %d-->%d\n",
		2*originalAsg.maxSize, originalAsg.maxSize, 2*originalAsg.desiredCapacity, originalAsg.desiredCapacity)
	err = setASGCapacities(ctx, sess, asgName, originalAsg.maxSize, originalAsg.desiredCapacity)
	if err != nil {
		// rollback !
		// TODO
		return err
	}

	fmt.Printf("Wait for scaling in\n")
	err = waitForInstanceCount(ctx, sess, asgName, int(originalAsg.desiredCapacity))
	if err != nil {
		// rollback !
		// TODO
		return err
	}

	// unprotect the new instances
	fmt.Printf("Remove instance protection on the new instances\n")
	err = removeInstanceProtection(ctx, sess, asgName, newInstances)
	if err != nil {
		// rollback !
		// TODO
	}

	fmt.Printf("Done in %s.\n", time.Since(start))

	return nil
}

type asg struct {
	instances       []instance
	tg              *targetGroup
	maxSize         int64
	desiredCapacity int64
}

type instance struct {
	instanceId     string
	lifecycleState string
	healthStatus   string
}

func (i instance) String() string {
	return fmt.Sprintf("%s lifecycle:%s health:%s", i.instanceId, i.lifecycleState, i.healthStatus)
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
		instances:       instances,
		tg:              tg,
		maxSize:         *out.AutoScalingGroups[0].MaxSize,
		desiredCapacity: *out.AutoScalingGroups[0].DesiredCapacity,
	}, nil
}

func setASGCapacities(ctx context.Context, sess *session.Session, asgName string, maxSize, desiredCapacity int64) error {
	as := autoscaling.New(sess)

	_, err := as.UpdateAutoScalingGroupWithContext(ctx, &autoscaling.UpdateAutoScalingGroupInput{
		AutoScalingGroupName: aws.String(asgName),
		DesiredCapacity:      aws.Int64(desiredCapacity),
		MaxSize:              aws.Int64(maxSize),
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

func waitForNewInstances(ctx context.Context, sess *session.Session, asgName string, count int, tg *targetGroup, currentInstances []instance) ([]instance, error) {
	newInstances, chanErr := detectNewInstances(ctx, sess, asgName, currentInstances)

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case err := <-chanErr:
				fmt.Printf("error while polling ASG details: %v\n", err)
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
			fmt.Printf("error while polling autoscaling or target group reported target health: %v\n", err)
			continue
		case inst := <-readyInstances:
			instanceReady = append(instanceReady, inst)
			fmt.Printf("\t(%d/%d) Instance %s is ready\n", len(instanceReady), count, inst.instanceId)
			if len(instanceReady) >= count {
				return instanceReady, nil
			}
		}
	}
}

func detectNewInstances(ctx context.Context, sess *session.Session, asgName string, currentInstances []instance) (chan instance, chan error) {
	oldSet := make(map[string]instance)
	for _, inst := range currentInstances {
		oldSet[inst.instanceId] = inst
	}

	chanOut := make(chan instance)
	chanErr := make(chan error)

	period := 5 * time.Second

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
				if _, has := oldSet[inst.instanceId]; !has {
					chanOut <- inst
					oldSet[inst.instanceId] = inst
				}
			}
		}
	}()

	return chanOut, chanErr
}

func detectInstancesReady(ctx context.Context, sess *session.Session, tg *targetGroup, newInstances chan instance) (chan instance, chan error) {
	chanOut := make(chan instance)
	chanErr := make(chan error)

	period := 10 * time.Second

	go func() {
		defer close(chanOut)
		defer close(chanErr)

		instanceSet := make(map[string]instance)

		for {
			select {
			case <-ctx.Done():
				return
			case inst := <-newInstances:
				fmt.Printf("\tfound new instance: %s\n", inst)
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
	period := 5 * time.Second

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

		fmt.Printf("\tInstance count: %d\n", len(asg.instances))
		if len(asg.instances) <= count {
			return nil
		}
	}
}
