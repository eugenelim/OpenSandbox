#!/usr/bin/env bash
# Tag every resource of an ade-spike eksctl cluster with the account tagging convention.
# Usage: ADE_TAG_KEY=<key> ADE_TAG_VALUE=<value> ADE_OWNER=<owner> ADE_EXPIRES=<yyyy-mm-dd> AWS_PROFILE=... AWS_REGION=... ./tag-resources.sh <cluster>
set -u; CL=${1:?cluster name}; R=${AWS_REGION:-us-east-1}
K=${ADE_TAG_KEY:?set ADE_TAG_KEY}; V=${ADE_TAG_VALUE:?set ADE_TAG_VALUE}; O=${ADE_OWNER:?set ADE_OWNER}; X=${ADE_EXPIRES:?set ADE_EXPIRES}
T="{\"$K\":\"$V\",\"Owner\":\"$O\",\"Project\":\"ade-spike\",\"Environment\":\"demo\",\"Expires\":\"$X\"}"
arns=$(aws resourcegroupstaggingapi get-resources --region "$R" --tag-filters "Key=alpha.eksctl.io/cluster-name,Values=$CL" --query 'ResourceTagMappingList[?!(Tags[?Key==`'"$K"'`])].ResourceARN' --output text | tr '\t' '\n')
arns2=$(aws resourcegroupstaggingapi get-resources --region "$R" --tag-filters "Key=aws:cloudformation:stack-name,Values=eksctl-$CL-cluster,eksctl-$CL-nodegroup-system,eksctl-$CL-nodegroup-metal,eksctl-$CL-nodegroup-nested,eksctl-$CL-nodegroup-kata" --query 'ResourceTagMappingList[?!(Tags[?Key==`'"$K"'`])].ResourceARN' --output text | tr '\t' '\n')
all=$(printf '%s\n%s\n' "$arns" "$arns2" | sort -u | grep -v '^$')
echo "$all" | wc -l | xargs echo "untagged resources:"
echo "$all" | xargs -n 20 sh -c 'aws resourcegroupstaggingapi tag-resources --region '"$R"' --resource-arn-list "$@" --tags '"'"'"$T"'"'"' --query "FailedResourcesMap" --output json' _
for role in $(aws iam list-roles --query "Roles[?starts_with(RoleName,'eksctl-$CL-')].RoleName" --output text); do aws iam tag-role --role-name "$role" --tags Key=$K,Value=$V Key=Owner,Value=$O Key=Project,Value=ade-spike Key=Environment,Value=demo Key=Expires,Value=$X; done
