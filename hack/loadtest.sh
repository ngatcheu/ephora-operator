#!/usr/bin/env bash
# Load test on a local kind cluster: creates N PreviewEnvironments at once,
# measures provisioning delays (DAT §6: < 3 min, >= 50 concurrent
# environments) and cleanup, then deletes everything.
#
# Prerequisites: kind cluster in the current context, dev CRD installed
# (`make install-dev`), operator running (`make deploy` or `make run`).
#
#   bash hack/loadtest.sh                # 50 environments, no pods
#   N=20 REPLICAS=1 bash hack/loadtest.sh
#
# REPLICAS=0 (default) measures the OPERATOR (namespace, guardrails, git
# clone, helm install) without depending on the node's capacity to schedule
# N pods. REPLICAS=1 also starts one nginx pod per environment.

set -euo pipefail

N="${N:-50}"
REPLICAS="${REPLICAS:-0}"
MGMT_NS="${MGMT_NS:-ephora-loadtest}"
TIMEOUT="${TIMEOUT:-900}"   # seconds
OPERATOR_NS="ephora-operator-system"
LABEL="ephora.io/loadtest=true"

case "$(kubectl config current-context)" in
  kind-*) ;;
  *) echo "Refusing: current context is not a kind cluster"; exit 1 ;;
esac

log() { printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*"; }

operator_rss() {
  local pod
  pod=$(kubectl -n "$OPERATOR_NS" get pod -l control-plane=controller-manager \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null) || return 0
  [ -n "$pod" ] || return 0
  kubectl -n "$OPERATOR_NS" exec "$pod" -- cat /proc/1/status 2>/dev/null \
    | awk '/VmRSS/ {printf "%.0f MiB", $2/1024}'
}

percentile() { # percentile <p> <sorted values...>
  local p=$1; shift
  local n=$#
  local idx=$(( (p * n + 99) / 100 ))
  [ "$idx" -lt 1 ] && idx=1
  eval "echo \${$idx}"
}

# --- Setup -----------------------------------------------------------------

kubectl create namespace "$MGMT_NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null
RSS_BEFORE=$(operator_rss)
log "Creating $N PreviewEnvironments in $MGMT_NS (replicaCount=$REPLICAS)"

manifest=$(mktemp)
trap 'rm -f "$manifest"' EXIT
for i in $(seq 1 "$N"); do
  pr=$((1000 + i))
  cat >> "$manifest" <<EOF
---
apiVersion: ephora.io/v1alpha1
kind: PreviewEnvironment
metadata:
  name: pr-${pr}-load
  namespace: ${MGMT_NS}
  labels:
    ephora.io/loadtest: "true"
spec:
  source:
    repo: "https://github.com/helm/examples"
    chartPath: "charts/hello-world"
    revision: "main"
  prNumber: ${pr}
  appName: load
  ttl: 1h
  valuesOverride:
    replicaCount: ${REPLICAS}
EOF
done

T0=$(date +%s)
kubectl apply -f "$manifest" >/dev/null
log "Applied in $(( $(date +%s) - T0 ))s — waiting for Running (timeout ${TIMEOUT}s)"

# --- Provisioning ----------------------------------------------------------

while :; do
  phases=$(kubectl -n "$MGMT_NS" get penv -l "$LABEL" -o jsonpath='{range .items[*]}{.status.phase}{"\n"}{end}')
  running=$(grep -c '^Running$' <<<"$phases" || true)
  failed=$(grep -c '^Failed$' <<<"$phases" || true)
  elapsed=$(( $(date +%s) - T0 ))
  printf '\r  %3ds  Running %d/%d  Failed %d   ' "$elapsed" "$running" "$N" "$failed"
  [ "$running" -ge "$N" ] && break
  if [ "$elapsed" -ge "$TIMEOUT" ]; then echo; log "TIMEOUT"; break; fi
  sleep 2
done
echo
T_ALL_RUNNING=$(( $(date +%s) - T0 ))
RSS_PEAK=$(operator_rss)

# Per-environment delay: creationTimestamp -> Ready=True transition.
mapfile -t delays < <(
  kubectl -n "$MGMT_NS" get penv -l "$LABEL" -o jsonpath='{range .items[*]}{.metadata.creationTimestamp} {.status.conditions[?(@.type=="Ready")].status} {.status.conditions[?(@.type=="Ready")].lastTransitionTime}{"\n"}{end}' \
  | awk '$2=="True" {print $1, $3}' \
  | while read -r created ready; do
      echo $(( $(date -d "$ready" +%s) - $(date -d "$created" +%s) ))
    done | sort -n
)

# --- Cleanup ---------------------------------------------------------------

log "Deleting all load-test environments"
T1=$(date +%s)
kubectl -n "$MGMT_NS" delete penv -l "$LABEL" --wait=false >/dev/null
while :; do
  left=$(kubectl -n "$MGMT_NS" get penv -l "$LABEL" --no-headers 2>/dev/null | wc -l)
  ns_left=$(kubectl get ns --no-headers 2>/dev/null | grep -c 'preview-pr-1[0-9][0-9][0-9]-load' || true)
  elapsed=$(( $(date +%s) - T1 ))
  printf '\r  %3ds  PreviewEnvironments left %d  namespaces left %d   ' "$elapsed" "$left" "$ns_left"
  [ "$left" -eq 0 ] && [ "$ns_left" -eq 0 ] && break
  if [ "$elapsed" -ge "$TIMEOUT" ]; then echo; log "TIMEOUT during cleanup"; break; fi
  sleep 3
done
echo
T_CLEANUP=$(( $(date +%s) - T1 ))
kubectl delete namespace "$MGMT_NS" --wait=false >/dev/null

# --- Report ----------------------------------------------------------------

n_ok=${#delays[@]}
echo
echo "================ ephora-operator load test ================"
printf '  Environments requested      %d (replicaCount=%s)\n' "$N" "$REPLICAS"
printf '  Reached Running             %d\n' "$n_ok"
if [ "$n_ok" -gt 0 ]; then
  printf '  Provisioning delay  p50     %ss\n' "$(percentile 50 "${delays[@]}")"
  printf '                      p95     %ss   (target < 180s)\n' "$(percentile 95 "${delays[@]}")"
  printf '                      max     %ss\n' "${delays[$((n_ok - 1))]}"
fi
printf '  All Running after           %ss\n' "$T_ALL_RUNNING"
printf '  Full cleanup after          %ss\n' "$T_CLEANUP"
printf '  Operator memory (RSS)       %s before, %s at peak\n' "${RSS_BEFORE:-n/a}" "${RSS_PEAK:-n/a}"
echo "============================================================"
