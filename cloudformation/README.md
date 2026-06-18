# VSAT Cluster v2 CloudFormation

This directory contains a standalone template for launching a fresh public Ubuntu
EC2 host for VSAT Cluster v2. The stack creates:

- one EC2 instance from Canonical's public Ubuntu 24.04 SSM AMI parameter
- an 8 GiB encrypted gp3 root volume by default
- one newly created encrypted gp3 EBS volume attached as a raw, unformatted block
  device for the future LXD COW btrfs pool
- a security group with parameterized SSH and HTTPS ingress

The COW volume is intentionally not formatted or mounted by CloudFormation. It is
tagged with `Purpose=vsat-lxd-cow` and `Filesystem=unformatted` so bootstrap logic
can discover and claim it later.

## Launch

Use `us-east-2` unless you have a reason to choose another region. The template is
configurable for another region as long as the Ubuntu SSM parameter exists there.

```bash
aws cloudformation create-stack \
  --stack-name vsat-cluster-v2-host \
  --template-body file://cloudformation/vsat-ubuntu-ec2.yaml \
  --parameters \
    ParameterKey=VpcId,ParameterValue=vpc-xxxxxxxx \
    ParameterKey=SubnetId,ParameterValue=subnet-xxxxxxxx \
    ParameterKey=KeyName,ParameterValue=my-key-pair \
    ParameterKey=SshIngressCidr,ParameterValue=203.0.113.10/32 \
    ParameterKey=HttpsIngressCidr,ParameterValue=0.0.0.0/0 \
  --profile Venafi-SE-Basic-Access-427380916706 \
  --region us-east-2
```

After the stack reaches `CREATE_COMPLETE`, SSH to the instance as `ubuntu` and run:

```bash
curl -fsSL https://raw.githubusercontent.com/tall27/vsat-cluster-v2/master/scripts/quickstart.sh | sudo bash
```

## Validate With AWS

Local YAML parsing can catch syntax problems, but CloudFormation's full validation
requires AWS API access:

```bash
aws cloudformation validate-template \
  --template-body file://cloudformation/vsat-ubuntu-ec2.yaml \
  --profile Venafi-SE-Basic-Access-427380916706 \
  --region us-east-2
```

## Important Parameters

| Parameter | Default | Purpose |
|---|---:|---|
| `InstanceType` | `t3a.medium` | EC2 shape for the host |
| `UbuntuAmiId` | Canonical Ubuntu 24.04 SSM path | Region-local Ubuntu AMI lookup |
| `RootVolumeSizeGiB` | `8` | Root gp3 disk size |
| `RootVolumeIops` | `3000` | Root gp3 IOPS |
| `RootVolumeThroughput` | `125` | Root gp3 throughput in MiB/s |
| `CowVolumeSizeGiB` | `40` | Attached raw EBS volume size |
| `CowVolumeIops` | `3000` | COW gp3 IOPS |
| `CowVolumeThroughput` | `125` | COW gp3 throughput in MiB/s |
| `CowVolumeDeviceName` | `/dev/sdf` | Requested EC2 attachment device |

## Outputs

The stack outputs the instance ID, public IP, public DNS name, COW EBS volume ID,
security group ID, and the quickstart command.
