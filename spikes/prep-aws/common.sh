# Sourced. Refuses to run against an unnamed account.
need() { command -v "$1" >/dev/null 2>&1 || { echo "missing tool: $1"; exit 1; }; }
guard_aws() {
  [ "${ADE_AWS_CONFIRM:-}" = yes ] || { echo "set ADE_AWS_CONFIRM=yes"; exit 1; }
  [ -n "${AWS_PROFILE:-}" ] && [ -n "${AWS_REGION:-}" ] && [ -n "${ADE_AWS_ACCOUNT:-}" ] || { echo "set AWS_PROFILE, AWS_REGION and ADE_AWS_ACCOUNT"; exit 1; }
  for t in aws eksctl kubectl; do need $t; done
  acct=$(aws sts get-caller-identity --query Account --output text) || exit 1
  [ "$acct" = "$ADE_AWS_ACCOUNT" ] || { echo "account mismatch: logged in as $acct, expected $ADE_AWS_ACCOUNT"; exit 1; }
  echo "account $acct region $AWS_REGION profile $AWS_PROFILE"
}
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RES="$HERE/results"; TS=$(date +%Y%m%d-%H%M%S)
# Egress policy probe run inside the app container of a sidecar pod: same checks as spike S-2.
egress_probe() { # $1 namespace $2 pod
  local ns=$1 pod=$2
  for c in \
   "allowed_pypi:curl -s -m 8 -o /dev/null -w %{http_code} https://pypi.org/simple/" \
   "unlisted_example_com:curl -s -m 8 -o /dev/null -w %{http_code} https://example.com/" \
   "unlisted_dns:getent hosts example.com || echo dns-failed" \
   "direct_ip_1.1.1.1:curl -s -m 6 -o /dev/null -w %{http_code} http://1.1.1.1/" \
   "metadata_169.254.169.254:curl -s -m 5 -o /dev/null -w %{http_code} http://169.254.169.254/" ; do
    name=${c%%:*}; cmd=${c#*:}
    printf '%s => ' "$name"; kubectl -n "$ns" exec "$pod" -c app -- sh -c "$cmd; echo \" rc=\$?\"" 2>&1 | tr '\n' ' '; echo
  done
}

# Fill placeholders in an eksctl config: region plus the account tagging convention and optional nested launch template id.
render_eksctl() { # $1 in $2 out
  : "${ADE_TAG_KEY:?set ADE_TAG_KEY}" "${ADE_TAG_VALUE:?set ADE_TAG_VALUE}" "${ADE_OWNER:?set ADE_OWNER}" "${ADE_EXPIRES:?set ADE_EXPIRES}"
  sed -e "s/REGION_PLACEHOLDER/$AWS_REGION/g" -e "s/TAG_KEY_PLACEHOLDER/$ADE_TAG_KEY/g" -e "s/TAG_VALUE_PLACEHOLDER/$ADE_TAG_VALUE/g" \
      -e "s/OWNER_PLACEHOLDER/$ADE_OWNER/g" -e "s/EXPIRES_PLACEHOLDER/$ADE_EXPIRES/g" -e "s/NESTED_LT_ID_PLACEHOLDER/${ADE_NESTED_LT_ID:-unset}/g" "$1" > "$2"
}
