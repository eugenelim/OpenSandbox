#!/usr/bin/env bash
# Private image path for an eksctl cluster whose open egress is revoked by organization auto-remediation:
# ECR interface endpoints, S3 gateway endpoint, S3 prefix-list egress rule, and a pull-through cache for public ECR.
# Usage: ADE_TAG_KEY=.. ADE_TAG_VALUE=.. ADE_OWNER=.. ADE_EXPIRES=.. AWS_PROFILE=.. ./egress-endpoints.sh up|down <cluster> | warm <repo-path> <tag>
set -u; A=${1:?up|down|warm}; CL=${2:-}; R=${AWS_REGION:-us-east-1}; export AWS_PAGER=""
ACCT=$(aws sts get-caller-identity --query Account --output text); REG="$ACCT.dkr.ecr.$R.amazonaws.com"
vpc() { aws ec2 describe-vpcs --filters "Name=tag:alpha.eksctl.io/cluster-name,Values=$CL" --query 'Vpcs[0].VpcId' --output text; }
case "$A" in
up)
  K=${ADE_TAG_KEY:?}; V=${ADE_TAG_VALUE:?}; O=${ADE_OWNER:?}; X=${ADE_EXPIRES:?}
  VPC=$(vpc); SG=$(aws eks describe-cluster --name "$CL" --query cluster.resourcesVpcConfig.clusterSecurityGroupId --output text)
  mapfile -t SUBS < <(aws ec2 describe-instances --filters "Name=vpc-id,Values=$VPC" Name=instance-state-name,Values=running --query 'Reservations[].Instances[].SubnetId' --output text | tr '\t' '\n' | sort -u)
  mapfile -t RTS < <(aws ec2 describe-route-tables --filters "Name=vpc-id,Values=$VPC" --query 'RouteTables[].RouteTableId' --output text | tr '\t' '\n')
  TAGS="ResourceType=vpc-endpoint,Tags=[{Key=$K,Value=$V},{Key=Owner,Value=$O},{Key=Project,Value=ade-spike},{Key=Environment,Value=demo},{Key=Expires,Value=$X}]"
  for S in ecr.api ecr.dkr; do aws ec2 create-vpc-endpoint --vpc-id "$VPC" --vpc-endpoint-type Interface --service-name "com.amazonaws.$R.$S" --subnet-ids "${SUBS[@]}" --security-group-ids "$SG" --private-dns-enabled --tag-specifications "$TAGS" --query 'VpcEndpoint.VpcEndpointId' --output text; done
  aws ec2 create-vpc-endpoint --vpc-id "$VPC" --vpc-endpoint-type Gateway --service-name "com.amazonaws.$R.s3" --route-table-ids "${RTS[@]}" --tag-specifications "$TAGS" --query 'VpcEndpoint.VpcEndpointId' --output text
  PL=$(aws ec2 describe-prefix-lists --filters "Name=prefix-list-name,Values=com.amazonaws.$R.s3" --query 'PrefixLists[0].PrefixListId' --output text)
  aws ec2 authorize-security-group-egress --group-id "$SG" --ip-permissions "IpProtocol=tcp,FromPort=443,ToPort=443,PrefixListIds=[{PrefixListId=$PL,Description=s3-gateway-endpoint}]" --query Return --output text
  aws ecr create-pull-through-cache-rule --ecr-repository-prefix ecr-public --upstream-registry-url public.ecr.aws --query ecrRepositoryPrefix --output text 2>&1 | tail -1
  echo "registry: $REG (pull public images as $REG/ecr-public/<path>:<tag> after 'warm')";;
warm) # one registry GET with the caller's credentials makes ECR import the upstream image
  curl -s -o /dev/null -w "warm $2:$3 -> %{http_code}\n" -u "AWS:$(aws ecr get-login-password --region "$R")" -H "Accept: application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json" "https://$REG/v2/ecr-public/$2/manifests/$3";;
down)
  VPC=$(vpc); mapfile -t E < <(aws ec2 describe-vpc-endpoints --filters "Name=vpc-id,Values=$VPC" --query 'VpcEndpoints[].VpcEndpointId' --output text | tr '\t' '\n')
  [ -n "${E[0]:-}" ] && aws ec2 delete-vpc-endpoints --vpc-endpoint-ids "${E[@]}" --query Unsuccessful --output text
  for REPO in $(aws ecr describe-repositories --query "repositories[?starts_with(repositoryName,'ecr-public/')].repositoryName" --output text); do aws ecr delete-repository --repository-name "$REPO" --force --query repository.repositoryName --output text; done
  aws ecr delete-pull-through-cache-rule --ecr-repository-prefix ecr-public --query ecrRepositoryPrefix --output text 2>&1 | tail -1;;
esac
