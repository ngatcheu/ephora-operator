#!/usr/bin/env bash
# Waits until a PreviewEnvironment reflects its latest spec, then prints a
# Markdown summary for the Pull Request comment (ADR-08). Used by the
# GitHub Action .github/actions/preview-comment; also runnable locally:
#
#   NAME=pr-42-demo-app bash hack/preview-status.sh
#   NAME=pr-42-demo-app MODE=deleted bash hack/preview-status.sh
#
# Variables: NAME (required), NAMESPACE (management namespace, default
# ephora-previews), MODE (deploy | deleted), TIMEOUT in seconds (default 600).
# Exit code: 0 ready (or deleted), 1 failed, 2 still provisioning at timeout.
# Requires kubectl and jq.

set -euo pipefail

NAME="${NAME:?NAME (PreviewEnvironment name) is required}"
NAMESPACE="${NAMESPACE:-ephora-previews}"
MODE="${MODE:-deploy}"
TIMEOUT="${TIMEOUT:-600}"
DOCS_URL="${DOCS_URL:-https://github.com/ngatcheu/ephora-operator/blob/main/docs/onboarding.md#dépannage}"

MARKER="<!-- ephora-preview:${NAME} -->"
FOOTER="<sub>Mis à jour automatiquement par ephora-operator · $(date -u '+%Y-%m-%d %H:%M UTC')</sub>"

if [ "${MODE}" = "deleted" ]; then
  printf '%s\n' "${MARKER}" \
    "### 🧹 Environnement de preview supprimé" "" \
    "La Pull Request est fermée : la release Helm et le namespace ont été supprimés." "" \
    "${FOOTER}"
  exit 0
fi

# The Ready condition carries the generation it was computed for: wait until
# it matches metadata.generation, so a stale status (previous commit) is never
# reported as the result of this run.
deadline=$(( $(date +%s) + TIMEOUT ))
while :; do
  json=$(kubectl -n "${NAMESPACE}" get previewenvironment "${NAME}" -o json 2>/dev/null || echo '{}')
  state=$(jq -r '
    (.metadata.generation // 0) as $gen
    | ([.status.conditions // [] | .[] | select(.type == "Ready")] | first) as $ready
    | if $ready == null or ($ready.observedGeneration // 0) < $gen then "pending"
      elif $ready.status == "True" then "ready"
      elif $ready.reason == "Provisioning" then "pending"
      else "failed" end' <<<"${json}")
  [ "${state}" != "pending" ] && break
  if [ "$(date +%s)" -ge "${deadline}" ]; then state="timeout"; break; fi
  sleep 5
done

field() { jq -r "${1} // \"\"" <<<"${json}"; }
ns=$(field '.status.namespace')
revision=$(field '.status.observedRevision')
[ -n "${revision}" ] || revision=$(field '.spec.source.revision')
reason=$(field '[.status.conditions // [] | .[] | select(.type == "Ready")] | first | .reason')
message=$(field '[.status.conditions // [] | .[] | select(.type == "Ready")] | first | .message')
expires=$(field '.status.expiresAt')
[ -n "${expires}" ] && expires=$(date -u -d "${expires}" '+%Y-%m-%d %H:%M UTC' 2>/dev/null || echo "${expires}")

case "${state}" in
  ready)
    printf '%s\n' "${MARKER}" \
      "### 🚀 Environnement de preview prêt" "" \
      "| | |" "|---|---|" \
      "| Statut | ✅ \`Running\` |" \
      "| Révision | \`${revision:0:12}\` |" \
      "| Namespace | \`${ns}\` |" \
      "| Expire le | ${expires} |" "" \
      "**Accès** (réseau interne) :" "" \
      '```bash' \
      "kubectl -n ${ns} get services" \
      "kubectl -n ${ns} port-forward svc/<service> 8080:80   # puis http://localhost:8080" \
      '```' "" \
      "${FOOTER}"
    exit 0 ;;
  failed)
    printf '%s\n' "${MARKER}" \
      "### ❌ Échec de l'environnement de preview" "" \
      "| | |" "|---|---|" \
      "| Raison | \`${reason}\` |" \
      "| Révision | \`${revision:0:12}\` |" \
      "| Namespace | \`${ns}\` |" "" \
      "~~~" "${message}" "~~~" "" \
      "L'opérateur réessaie automatiquement ; un nouveau commit relance immédiatement le déploiement." \
      "Voir le [guide de dépannage](${DOCS_URL})." "" \
      "${FOOTER}"
    exit 1 ;;
  timeout)
    printf '%s\n' "${MARKER}" \
      "### ⏳ Environnement de preview toujours en cours de création" "" \
      "Pas de résultat après ${TIMEOUT} s pour la révision \`${revision:0:12}\`." \
      "Suivi : \`kubectl -n ${NAMESPACE} describe previewenvironment ${NAME}\`" "" \
      "${FOOTER}"
    exit 2 ;;
esac
