# VSAT Cluster Stack Runbook

Use this file first when creating or updating a VSAT Cluster v2 stack.

## Active Templates

- Root template: `cloudformation/mainstack.yaml`
- Nested workload template: `cloudformation/vsatstack.yaml`
- Published S3 prefix: `s3://akush/vsat-cluster/`
- Root template URL: `https://akush.s3.us-east-2.amazonaws.com/vsat-cluster/mainstack.yaml`
- Older standalone reference only: `cloudformation/vsat-ubuntu-ec2.yaml`

## AWS Defaults

- Profile: `AdministratorAccess-427380916706`
- Region: `us-east-2`
- Key pair: `talk-vnfi-Ohio`
- Instance type: `t3a.xlarge`
- Root volume: `8` GiB
- LXD COW volume: `40` GiB, `3000` IOPS, `125` MiB/s

## Publish And Validate

```bash
aws s3 sync cloudformation/ s3://akush/vsat-cluster/ \
  --profile AdministratorAccess-427380916706 \
  --region us-east-2

aws cloudformation validate-template \
  --template-url https://akush.s3.us-east-2.amazonaws.com/vsat-cluster/mainstack.yaml \
  --profile AdministratorAccess-427380916706 \
  --region us-east-2
```

## Create Stack

```bash
aws cloudformation create-stack \
  --stack-name vsat-cluster-cow-<suffix> \
  --template-url https://akush.s3.us-east-2.amazonaws.com/vsat-cluster/mainstack.yaml \
  --parameters \
    ParameterKey=CustomerName,ParameterValue=vsat-cow-<suffix> \
    ParameterKey=AdminCidr,ParameterValue=<your-public-ip>/32 \
    ParameterKey=KeyName,ParameterValue=talk-vnfi-Ohio \
    ParameterKey=InstanceType,ParameterValue=t3a.xlarge \
    ParameterKey=RootVolumeSizeGiB,ParameterValue=8 \
    ParameterKey=CowVolumeSizeGiB,ParameterValue=40 \
    ParameterKey=CowVolumeIops,ParameterValue=3000 \
    ParameterKey=CowVolumeThroughput,ParameterValue=125 \
  --capabilities CAPABILITY_NAMED_IAM CAPABILITY_AUTO_EXPAND \
  --profile AdministratorAccess-427380916706 \
  --region us-east-2
```

Use the `SshOverSsmCommand` stack output for SSH through Session Manager when
direct port 22 should remain closed. It uses the EC2 instance ID as the SSH host
and `AWS-StartSSHSession` as the proxy command.

## Check Stack

```bash
aws cloudformation describe-stacks \
  --stack-name vsat-cluster-cow-<suffix> \
  --profile AdministratorAccess-427380916706 \
  --region us-east-2
```
