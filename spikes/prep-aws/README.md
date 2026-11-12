# AWS spikes: can the egress sidecar run without nodes to patch, and does gVisor work on EKS nodes?

Status: Fargate F1-F4 and Kata-on-EKS runs done; F5 and the gVisor spike are not valid or not run. Results live in the knowledge base, not in this repository.

## Why these two
OpenSandbox's Kubernetes egress sidecar is added to every sandbox pod with `securityContext.capabilities.add: [NET_ADMIN]` (`server/opensandbox_server/services/k8s/egress_helper.py`), and the main container drops it. With `egress.disable_ipv6 = true` the execd init container runs `privileged: true` (default is false). AWS's ECS Fargate guidance says `CAP_NET_ADMIN` and privileged containers are restricted; EKS Fargate wording was not found, so it must be tested. Separately, gVisor's netfilter support is limited, so `dns+nft` under gVisor on Kubernetes is untested (S-2).

| Spike | Question | Decides |
|---|---|---|
| **F** Fargate | Can a pod on EKS Fargate get `NET_ADMIN` and run the egress sidecar (`dns+nft`, then `dns`)? Is a privileged init container admitted? | Whether the no-node-patching option is real |
| **E** gVisor on an EKS node | Does the sidecar enforce policy for a pod running under runsc? Is gVisor actually active? | Whether EKS + gVisor is the target; else Kata or accepted runc risk |

## Outcome table
| Result | Meaning | Document action |
|---|---|---|
| F1 `net-admin` pod shows `NET_ADMIN_DENIED` or is rejected | Fargate cannot host the sidecar | Drop Fargate for sandboxes; keep it only for control-plane services (Gateway) |
| F1 ok, F2 (`dns+nft`) enforces | Fargate works for sandboxes | EKS-on-Fargate becomes the lowest-maintenance end state; check pool and pod size limits next |
| F2 fails, F4 (`dns`) enforces | Only name-level filtering; direct-IP bypass possible | Not acceptable alone for untrusted code; test VPC-level controls (AWS Network Firewall domain rules, security-group egress limits) as a replacement design |
| F3 admitted | Privileged init works | Only matters if `disable_ipv6` is enabled |
| E1 `dns+nft` enforces and `dmesg` shows gVisor | EKS + gVisor + sidecar works | EKS with a gVisor node pool, patched by AMI replacement |
| E1 `dns+nft` fails, `dns` enforces | gVisor netfilter gap | Same concern as F4; or use Kata (needs KVM, bare metal or nested) |
| E1 pod stuck Pending / runtime error | Runtime not installed or registered | Fix the node bootstrap (see `eksctl/gvisor-nodegroup.userdata.sh`) and rerun |

## Spike F5: Fargate without the sidecar (added 2026-11-11, UNTESTED sketch)
`fargate-nosidecar-spike.sh` tests whether network-level controls can replace the sidecar where `NET_ADMIN` is unavailable: a pod with no sidecar and no capabilities uses an explicit allow-list proxy (squid on the managed node group), and a per-pool security group is meant to enforce it. Stage 1 is a baseline with direct egress still open; stage 2 attaches a restrictive security group. Known snags: Fargate pulls images over the pod's own network interface (stage 2 needs ECR endpoints in the VPC and a mirrored image); the VPC resolver stays reachable, so DNS tunnelling needs Route 53 Resolver DNS Firewall; security groups for Fargate pods need the SecurityGroupPolicy CRD. It does not test OpenSandbox creating sandboxes without a network policy; that is a follow-up.

| Result | Meaning | Document action |
|---|---|---|
| Stage 2: direct IP and public DNS fail, allowed proxy domain works, unlisted domain denied | Network-level enforcement works without `NET_ADMIN` | Fargate becomes viable for sandboxes with per-pool policy and explicit proxies (git, model, mirrors); the Vault's transparent injection is replaced by explicit proxies |
| Stage 2: pod cannot start or pull its image | Snags above bite | Add ECR endpoints and a mirrored image, rerun |
| Resolver long-label query returns an answer | DNS tunnelling channel open | Add Route 53 Resolver DNS Firewall before accepting this design |

## Prerequisites (not installed on this machine at the time of writing)
`eksctl` 0.231.0 and `aws` CLI 2.39.4 (installed 2026-11-09), `kubectl` (present), and an AWS **sandbox** account you may spend in. Cost: an EKS control plane bills hourly (check current pricing) plus one or two small nodes; run for about an hour and tear down.

## Safety
- Scripts refuse to run unless `ADE_AWS_CONFIRM=yes`, `AWS_PROFILE` and `AWS_REGION` are set, and `ADE_AWS_ACCOUNT` equals the account id returned by `aws sts get-caller-identity`.
- Nothing runs in an account you did not name. A trap deletes the cluster on exit unless `ADE_KEEP=yes`.
- Images: the egress image comes from Docker Hub in the manifests. For a corporate account mirror it to ECR and pin by digest first.

## Run
```
export AWS_PROFILE=<sandbox-profile> AWS_REGION=<region> ADE_AWS_ACCOUNT=<12-digit id> ADE_AWS_CONFIRM=yes
export ADE_TAG_KEY=<tag key> ADE_TAG_VALUE=<tag value> ADE_OWNER=<owner> ADE_EXPIRES=<yyyy-mm-dd>   # applied to every resource
./fargate-spike.sh      # F1-F4, writes results/fargate-<timestamp>.txt
./gvisor-spike.sh       # E1, writes results/gvisor-<timestamp>.txt
```
Record results in `../../13-spike-results.md` and update the outcome table above.

## Not covered
Kata or Firecracker on EKS (needs KVM: bare metal or nested virtualisation, instance types unconfirmed), Bedrock access (S-3), and full OpenSandbox on EKS via Helm (a later step once these two pass).
