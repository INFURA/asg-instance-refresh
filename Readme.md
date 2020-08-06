# asg-instance-refresh

`asg-instance-refresh` is a minimal CLI tool to trigger an [instance refresh](https://docs.aws.amazon.com/autoscaling/ec2/userguide/asg-instance-refresh.html) on a EC2 autoscaling group.

## Usage

`asg-instance-refresg <asg-name>`

## Flags

- `region` - The AWS region. Default to us-east-1.
- `warmup` - How long the new instances need to warm up. Default to the value for the health check grace period defined for the group.
- `minhealthy` - The minimum percentage capacity to retain during the recycling. Default to 100%.
- `wait` - Wait until the refresh is complete to return.

## License

MIT