# asg-instance-refresh

`asg-instance-refresh` is a minimal CLI tool to trigger an [instance refresh](https://docs.aws.amazon.com/autoscaling/ec2/userguide/asg-instance-refresh.html) on a EC2 autoscaling group.

## Installation

`GO111MODULE=off go get -u github.com/INFURA/asg-instance-refresh`

## Usage

`asg-instance-refresh <asg-name>`

## Flags

- `region` - The AWS region. Default to us-east-1.
- `strategy` - the selected refresh strategy. Default to "rolling".
- `warmup` - How long the new instances need to warm up. Default to the value for the health check grace period defined for the group.
- `minhealthy` - The minimum percentage capacity to retain during the recycling. Default to 100%.
- `wait` - Wait until the refresh is complete to return.

# Refresh strategies

`asg-instance-refresh` support the refresh strategies given by AWS. At the time of writing, only one strategy exist: `rolling`.

Because this `rolling` strategy is extremely slow, this tool support an additional strategy: `infura-refresh`. In short, this strategy works the following way:
- double the desired count
- wait for new healthy instances to appear
- enable scale-in protection on the new instances
- set back the desired count to normal
- wait for the old instances to get removed
- set the instance protection or not according to the ASG defined setting

## License

MIT
