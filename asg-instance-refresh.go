package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/autoscaling"
)

var (
	region     = flag.String("region", "us-east-1", "The AWS region. Default to us-east-1.")
	warmup     = flag.Int64("warmup", -1, `How long the new instances need to warm up. Default to the value for the health check grace period defined for the group.`)
	minHealthy = flag.Int64("minhealthy", 100, `The minimum percentage capacity to retain during the recycling. Default to 100%.`)
)

func main() {
	flag.Parse()

	args := flag.Args()
	if len(args) != 1 {
		_, _ = fmt.Fprintln(os.Stderr, "this command expect one argument: the ASG name")
		os.Exit(1)
	}
	asg := args[0]

	ses := session.Must(session.NewSession(aws.NewConfig().WithRegion(*region)))
	svc := autoscaling.New(ses)

	prefs := &autoscaling.RefreshPreferences{
		MinHealthyPercentage: minHealthy,
	}

	if *warmup > -1 {
		prefs.InstanceWarmup = warmup
	}

	input := &autoscaling.StartInstanceRefreshInput{
		AutoScalingGroupName: aws.String(asg),
		Preferences:          prefs,
	}

	result, err := svc.StartInstanceRefresh(input)
	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case autoscaling.ErrCodeLimitExceededFault:
				fmt.Println(autoscaling.ErrCodeLimitExceededFault, aerr.Error())
			case autoscaling.ErrCodeResourceContentionFault:
				fmt.Println(autoscaling.ErrCodeResourceContentionFault, aerr.Error())
			case autoscaling.ErrCodeInstanceRefreshInProgressFault:
				fmt.Println(autoscaling.ErrCodeInstanceRefreshInProgressFault, aerr.Error())
			default:
				fmt.Println(aerr.Error())
			}
		} else {
			// Print the error, cast err to awserr.Error to get the Code and
			// Message from an error.
			fmt.Println(err.Error())
		}
		// exit with error-ish code
		os.Exit(1)
	}

	fmt.Println("Refresh started with request ID", result.InstanceRefreshId)
}
