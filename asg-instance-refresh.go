package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
)

var ErrStrategyFailed = errors.New("strategy failed")

func main() {
	region := flag.String("region", "us-east-1", "The AWS region. Default to us-east-1.")
	strategy := flag.String("strategy", "rolling", fmt.Sprintf("The deploy strategy. Possible values are [%s]", strings.Join([]string{"rolling"}, ",")))
	warmup := flag.Int64("warmup", -1, `How long the new instances need to warm up. Default to the value for the health check grace period defined for the group.`)
	minHealthy := flag.Int64("minhealthy", 100, `The minimum percentage capacity to retain during the recycling. Default to 100%.`)
	wait := flag.Bool("wait", false, "Wait until the refresh is complete to return.")

	flag.Parse()

	args := flag.Args()
	if len(args) != 1 {
		_, _ = fmt.Fprintln(os.Stderr, "this command expect one argument: the ASG name")
		os.Exit(1)
	}
	asgName := args[0]

	var err error

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		select {
		case <-c:
			cancel()
		case <-ctx.Done():
		}
	}()

	switch strings.ToLower(*strategy) {
	case "rolling": // natively supported AWS strategies
		err = strategyAWS(ctx, *region, asgName, *strategy, *warmup, *minHealthy, *wait)
	case "infura":
		err = strategyInfuraRefresh(ctx, *region, asgName)
	default:
		_, _ = fmt.Fprintf(os.Stderr, "unknown strategy %s\n", *strategy)
		os.Exit(1)
	}

	if err != nil && err != ErrStrategyFailed {
		fmt.Println(err.Error())
	}
	if err != nil {
		// exit with error-ish code
		os.Exit(1)
	}
}
