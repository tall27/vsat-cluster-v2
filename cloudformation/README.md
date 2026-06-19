# VSAT Cluster v2 CloudFormation

This directory contains the CloudFormation package for launching a fresh public
Ubuntu EC2 host for VSAT Cluster v2.

The current stack follows the `onebox.v2` nested-stack pattern:

- `mainstack.yaml` - root stack that points child stacks at
  `s3://akush/vsat-cluster/`
- `vpcstack.yaml` - fresh VPC, public subnet, internet gateway, route table
- `sgstack.yaml` - SSH from `AdminCidr`, public HTTPS, internal VPC traffic
- `vsatstack.yaml` - Ubuntu EC2 instance, 8 GiB root, gp3 COW block device
  with delete-on-termination enabled, SSM Session Manager SSH proxy support,
  and UserData quickstart
- `vsat-ubuntu-ec2.yaml` - older standalone template kept for reference

The workload stack launches Ubuntu 24.04, creates the COW gp3 EBS volume as an
EC2 block device with delete-on-termination enabled, and runs this UserData
command after `/dev/nvme1n1` appears:

```bash
curl -fsSL https://raw.githubusercontent.com/tall27/vsat-cluster-v2/master/scripts/quickstart.sh | sudo VSAT_COW_DEVICE=/dev/nvme1n1 bash
```

Quickstart creates the required LXD `cow` btrfs pool on the attached EBS volume,
installs the web app, and starts `vsat-webapp.service`.

## Publish Templates

```bash
aws s3 sync cloudformation/ s3://akush/vsat-cluster/ \
  --profile Venafi-SE-Basic-Access-427380916706 \
  --region us-east-2
```

## Validate

```bash
aws cloudformation validate-template \
  --template-url https://akush.s3.us-east-2.amazonaws.com/vsat-cluster/mainstack.yaml \
  --profile Venafi-SE-Basic-Access-427380916706 \
  --region us-east-2
```

## Launch

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
  --profile Venafi-SE-Basic-Access-427380916706 \
  --region us-east-2
```

## Verified Test Stack

Test stack `vsat-cluster-cow-06180330` was created in `us-east-2` from the S3
root template. Verification showed:

- CloudFormation `CREATE_COMPLETE`
- UserData completed
- `vsat-webapp.service` active on port 443
- public `https://18.227.0.199/healthz` returned `200 OK`
- LXD storage pool `cow` exists with driver `btrfs`
- `vsat-nested` root disk uses pool `cow`
- `/dev/nvme1n1` is the 40 GiB attached COW volume

## Outputs

The root stack outputs the instance ID, public IP, public DNS name, raw COW volume
ID, direct SSH command, SSH-over-SSM command, and health URL. Use
`SshOverSsmCommand` when port 22 is not open to your current IP.
