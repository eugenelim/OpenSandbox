# ADE spike scripts

Throwaway scripts for evaluating OpenSandbox as the sandbox layer of a self-hosted coding-agent platform: relay and git-transport prototypes, egress and pool tests, a ref-aware git proxy, local Kata/gVisor benchmarks (`spikes/prep/`) and AWS EKS experiments (`spikes/prep-aws/`).

No account identifiers are stored here. AWS scripts read everything account-specific from environment variables (`AWS_PROFILE`, `AWS_REGION`, `ADE_AWS_ACCOUNT`, `ADE_AWS_CONFIRM`, and for resource tagging `ADE_TAG_KEY`, `ADE_TAG_VALUE`, `ADE_OWNER`, `ADE_EXPIRES`; `ADE_NESTED_LT_ID` for the nested-virtualisation node group) and refuse to run without them. Local scripts expect `OPENSANDBOX_REPO` to point at an OpenSandbox checkout. Results are not committed.
