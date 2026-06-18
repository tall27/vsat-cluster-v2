# Performance Notes

This file captures the current performance findings for the AWS VSAT Cluster v2
test host and the next tuning options to evaluate.

## Current AWS target

Use this AWS CLI profile for project work:

```bash
--profile Venafi-SE-Basic-Access-427380916706
```

## Verified COW-volume CloudFormation deployment

On 2026-06-18, the project was moved to a nested CloudFormation deployment
modeled after `onebox.v2`:

| Field | Value |
|---|---|
| S3 prefix | `s3://akush/vsat-cluster/` |
| Root template URL | `https://akush.s3.us-east-2.amazonaws.com/vsat-cluster/mainstack.yaml` |
| Stack | `vsat-cluster-cow-06180330` |
| Instance ID | `i-09b13b5b2fbb3df64` |
| Instance type | `t3a.medium` |
| Public IP | `18.227.0.199` |
| Root volume | `8 GiB gp3` |
| COW volume | `vol-004ced4a51b21efec`, `40 GiB gp3`, attached as `/dev/nvme1n1` |
| UserData | `curl -fsSL https://raw.githubusercontent.com/tall27/vsat-cluster-v2/master/scripts/quickstart.sh \| sudo VSAT_COW_DEVICE=/dev/nvme1n1 bash` |

Verification results:

- CloudFormation reached `CREATE_COMPLETE`.
- UserData completed successfully.
- `vsat-webapp.service` was active on `:443`.
- Public `https://18.227.0.199/healthz` returned `200 OK`.
- Public `/` returned `401 Unauthorized`, confirming the UI was reachable and
  auth was enforced.
- LXD storage pool `cow` exists with driver `btrfs`.
- `vsat-nested` root disk uses pool `cow`.
- `/dev/nvme1n1` is the 40 GiB attached btrfs COW volume.

The important conclusion is that COW storage should be treated as required
infrastructure, not an optional fallback. `scripts/bootstrap-host.sh` now fails
fast if it cannot find or use a dedicated unformatted COW disk.

Known instance:

| Field | Value |
|---|---|
| Instance ID | `i-04a6da1c4d8009e15` |
| Name | `[Tal] vsat cluster Jun-9` |
| Instance type | `t3a.medium` |
| Private IP | `10.0.2.38` |
| Root volume | `vol-00fd25bf7ab44d3ed` |

The instance was stopped during the latest disk check, so it had no public IP at
that time.

## Current disk

The attached root disk is:

| Field | Value |
|---|---|
| Device | `/dev/sda1` |
| Type | `gp3` |
| Size | `30 GiB` |
| Provisioned IOPS | `3000` |
| Provisioned throughput | `125 MiB/s` |
| Encrypted | `true` |

## Instance-side EBS limits

For `t3a.medium`, AWS reports:

| Limit | Value |
|---|---|
| Baseline EBS IOPS | `2000` |
| Baseline EBS throughput | `43.75 MB/s` |
| Maximum EBS IOPS | `11800` |
| Maximum EBS throughput | `260.625 MB/s` |

For comparison, `t3a.large` reports:

| Limit | Value |
|---|---|
| Baseline EBS IOPS | `4000` |
| Baseline EBS throughput | `86.875 MB/s` |
| Maximum EBS IOPS | `15700` |
| Maximum EBS throughput | `347.5 MB/s` |

Important interpretation: the current gp3 root volume is configured for
`3000 IOPS / 125 MiB/s`, but sustained performance can still be constrained by
the `t3a.medium` instance-side EBS baseline.

## Observed stall behavior

During the web app stall investigation, the host at `3.23.63.89` accepted TCP
connections but did not respond promptly:

- SSH reached port `22`, then timed out during banner exchange.
- HTTPS reached port `443`, then returned zero bytes until client timeout.
- AWS instance status checks still passed.
- SSM showed the instance as online, but a read-only diagnostic command stayed
  pending.
- CloudWatch CPU was sustained around `70-80%`.
- CPU credit balance was `0.0`.
- EC2 console output showed kernel scheduler starvation:
  - `rcu_sched detected stalls`
  - `rcu_sched kthread starved`
  - `Unless rcu_sched kthread gets sufficient CPU time, OOM is now expected behavior.`

This points to host-level CPU starvation or a wedged userspace under provisioning
load, not just a web application request-path bug.

## Workload shape

The machine is mostly idle after provisioning completes. The stress happens
during container creation and the initial application install inside each
container, especially while Kubernetes and the Venafi backend are installed.

This means the goal is to improve short provisioning bursts without necessarily
paying for a permanently larger steady-state instance.

## Recommendations to evaluate

### 1. Serialize provisioning

Only run one Kubernetes/Venafi install at a time on `t3a.medium`.

The app should avoid overlapping heavy container provisioning work. A simple
queue or operational discipline may be enough: wait for one container install to
settle before starting the next.

### 2. Add a dedicated gp3 volume for LXD

Move LXD storage off the root filesystem. This should reduce contention between
the OS/app/service logs and container unpack/install IO.

Suggested starting point:

| Setting | Value |
|---|---|
| Volume type | `gp3` |
| Size | `40-100 GiB` |
| IOPS | `3000-6000` |
| Throughput | `125-250 MiB/s` |

Do not over-provision far beyond this on `t3a.medium`; the instance cannot fully
use very high gp3 settings under sustained load.

The verified CloudFormation test used `40 GiB / 3000 IOPS / 125 MiB/s` on a
separate gp3 volume and successfully created the LXD `cow` btrfs pool on
`/dev/nvme1n1`.

### 3. Require LXD copy-on-write storage

Confirm LXD uses the btrfs `cow` pool created by `scripts/bootstrap-host.sh`.
Avoid the default `dir` storage driver for repeated container launches, because
it does full filesystem copies and can make provisioning much slower.

### 4. Protect host CPU during install

The host has only 2 vCPUs. If a provisioning container can consume both, SSH,
SSM, the web app, LXD, journald, and kernel housekeeping can all be starved.

Options to evaluate:

- Limit active provisioning to one container.
- Temporarily set lower CPU limits for containers during install.
- Give the host/app enough CPU headroom so management paths remain responsive.

### 5. Watch CPU credits

The previous stall coincided with `CPUCreditBalance=0`. Faster disk will not fix
responsiveness if the instance is CPU-credit starved.

Options:

- Enable T3 Unlimited for short provisioning windows, accepting possible surplus
  credit charges.
- Temporarily resize to `t3a.large` during bulk provisioning, then resize back to
  `t3a.medium` for steady state.
- Move to a non-burstable instance family if provisioning stability matters more
  than minimum idle cost.

## Practical next experiment

1. Attach a dedicated gp3 EBS volume for LXD.
2. Configure LXD to use that volume as the COW storage pool.
3. Provision one container at a time.
4. During provisioning, watch:
   - CPU credit balance
   - CPU utilization
   - SSH responsiveness
   - HTTPS responsiveness
   - LXD operation latency
   - EBS queue length and throughput, if CloudWatch permissions allow it
5. If the host still becomes unresponsive, test a temporary `t3a.large` resize for
   the provisioning phase.

## Useful commands

```bash
aws ec2 describe-instances \
  --profile Venafi-SE-Basic-Access-427380916706 \
  --instance-ids i-04a6da1c4d8009e15

aws ec2 describe-volumes \
  --profile Venafi-SE-Basic-Access-427380916706 \
  --volume-ids vol-00fd25bf7ab44d3ed

aws ec2 describe-instance-types \
  --profile Venafi-SE-Basic-Access-427380916706 \
  --instance-types t3a.medium t3a.large
```
