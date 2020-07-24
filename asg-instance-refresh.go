package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/autoscaling"
)

var (
	region     = flag.String("region", "us-east-1", "The AWS region. Default to us-east-1.")
	warmup     = flag.Int64("warmup", -1, `How long the new instances need to warm up. Default to the value for the health check grace period defined for the group.`)
	minHealthy = flag.Int64("minhealthy", 100, `The minimum percentage capacity to retain during the recycling. Default to 100%.`)
	wait       = flag.Bool("wait", false, "Wait until the refresh is complete to return.")
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
		fmt.Println(err.Error())
		// exit with error-ish code
		os.Exit(1)
	}

	refreshId := *result.InstanceRefreshId

	fmt.Printf("Refresh started on ASG %s with request ID %s\n", asg, refreshId)

	if !*wait {
		return
	}

	start := time.Now()
	for {
		time.Sleep(5 * time.Second)

		result, err := svc.DescribeInstanceRefreshes(&autoscaling.DescribeInstanceRefreshesInput{
			AutoScalingGroupName: aws.String(asg),
			InstanceRefreshIds:   []*string{result.InstanceRefreshId},
		})
		if err != nil {
			fmt.Println(err.Error())
			os.Exit(1)
		}

		if len(result.InstanceRefreshes) != 1 {
			fmt.Printf("Found %v refresh with ID %v\n", len(result.InstanceRefreshes), refreshId)
			os.Exit(1)
		}

		refresh := result.InstanceRefreshes[0]
		switch *refresh.Status {
		case "Successful":
			fmt.Printf("Refresh completed in %v\n", refresh.EndTime.Sub(*refresh.StartTime).Truncate(time.Second))
			return

		case "Pending", "InProgress":
			fmt.Printf("%v\tRefresh is %v\n", time.Since(start).Truncate(time.Second), *refresh.Status)

		case "Failed", "Cancelling", "Cancelled":
			fmt.Printf("Refresh is %v\n", *refresh.Status)
			os.Exit(1)

		default:
			fmt.Printf("Unknown refresh status %v\n", *refresh.Status)
			os.Exit(1)
		}
	}
}
