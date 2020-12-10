package main

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/autoscaling"
)

func strategyAWS(ctx context.Context, region string, asgName string, strategy string, warmup int64, minHealthy int64, wait bool) error {
	ses := session.Must(session.NewSession(aws.NewConfig().WithRegion(region)))
	svc := autoscaling.New(ses)

	prefs := &autoscaling.RefreshPreferences{
		MinHealthyPercentage: aws.Int64(minHealthy),
	}

	if warmup > -1 {
		prefs.InstanceWarmup = aws.Int64(warmup)
	}

	input := &autoscaling.StartInstanceRefreshInput{
		AutoScalingGroupName: aws.String(asgName),
		Strategy:             aws.String(strategy),
		Preferences:          prefs,
	}

	result, err := svc.StartInstanceRefreshWithContext(ctx, input)
	if err != nil {
		return err
	}

	refreshId := *result.InstanceRefreshId

	fmt.Printf("Refresh (strategy: AWS native %s) started on ASG %s with request ID %s\n", strategy, asgName, refreshId)

	if !wait {
		return nil
	}

	start := time.Now()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}

		result, err := svc.DescribeInstanceRefreshesWithContext(ctx, &autoscaling.DescribeInstanceRefreshesInput{
			AutoScalingGroupName: aws.String(asgName),
			InstanceRefreshIds:   []*string{result.InstanceRefreshId},
		})
		if err != nil {
			return err
		}

		if len(result.InstanceRefreshes) != 1 {
			fmt.Printf("Found %v refresh with ID %v\n", len(result.InstanceRefreshes), refreshId)
			return ErrStrategyFailed
		}

		refresh := result.InstanceRefreshes[0]
		switch *refresh.Status {
		case "Successful":
			fmt.Printf("Refresh completed in %v\n", refresh.EndTime.Sub(*refresh.StartTime).Truncate(time.Second))
			return nil

		case "Pending", "InProgress":
			fmt.Printf("%v\tRefresh is %v\n", time.Since(start).Truncate(time.Second), *refresh.Status)

		case "Failed", "Cancelling", "Cancelled":
			fmt.Printf("Refresh is %v\n", *refresh.Status)
			return ErrStrategyFailed

		default:
			fmt.Printf("Unknown refresh status %v\n", *refresh.Status)
			return ErrStrategyFailed
		}
	}
}
