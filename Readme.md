# asg-instance-refresh

`asg-instance-refresh` is a minimal CLI tool to trigger an [instance refresh](https://docs.aws.amazon.com/autoscaling/ec2/userguide/asg-instance-refresh.html) on a EC2 autoscaling group.

## Usage

`asg-instance-refresg <asg-name>`

## Flags

- `region` - The AWS region. Default to us-east-1.
- `warmup` - How long the new instances need to warm up. Default to the value for the health check grace period defined for the group.
- `minhealthy` - The minimum percentage capacity to retain during the recycling. Default to 100%.

## Notes

This feature already exist in the AWS CLI tool but only on the V2 version which is not commonly packaged at the moment. The UX is also terrible (it reads a JSON file instead of using flags). Once those problems are resolved, this could possibly be retired.