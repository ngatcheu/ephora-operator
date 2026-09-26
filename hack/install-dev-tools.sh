#!/usr/bin/env bash
# Installe la boîte à outils de développement d'opérateurs Kubernetes sous Linux / WSL.
# Indépendant de tout projet : réutilisable pour n'importe quel opérateur
# (Kubebuilder, Operator SDK Go/Helm/Ansible).
#
# Pas besoin de sudo : tout est installé dans ~/.local (binaires dans ~/.local/bin,
# Go dans ~/.local/go). Idempotent : un outil déjà présent n'est pas réinstallé.
#
# Usage :
#   bash install-dev-tools.sh              # installe ce qui manque
#   bash install-dev-tools.sh --upgrade    # réinstalle tout en dernière version
#
# Versions : dernière version stable par défaut, surchargeable par variable
# d'environnement, par exemple :
#   GO_VERSION=go1.26.2 HELM_VERSION=v3.19.0 bash install-dev-tools.sh
#
# Variable optionnelle : GITHUB_TOKEN, pour éviter la limite de 60 requêtes/h
# de l'API GitHub non authentifiée.

set -euo pipefail

PREFIX="${PREFIX:-${HOME}/.local}"
BIN_DIR="${PREFIX}/bin"
GO_ROOT="${PREFIX}/go"
UPGRADE=false
[ "${1:-}" = "--upgrade" ] && UPGRADE=true

OS="linux"
ARCH="$(uname -m | sed 's/x86_64/amd64/; s/aarch64/arm64/')"
TMP="$(mktemp -d)"
trap 'rm -rf "${TMP}"' EXIT

mkdir -p "${BIN_DIR}"
export PATH="${GO_ROOT}/bin:${HOME}/go/bin:${BIN_DIR}:${PATH}"

# --- Utilitaires ---------------------------------------------------------

log() { printf '%-10s %s\n' "[$1]" "$2"; }

# needs_install <commande> : vrai si l'outil est absent ou si --upgrade est demandé.
needs_install() {
    if ! ${UPGRADE} && command -v "$1" >/dev/null 2>&1; then
        log ok "$1"
        return 1
    fi
    return 0
}

# gh_latest <owner/repo> [préfixe de tag] : dernier tag de release GitHub.
gh_latest() {
    local repo="$1" prefix="${2:-}" auth=()
    [ -n "${GITHUB_TOKEN:-}" ] && auth=(-H "Authorization: Bearer ${GITHUB_TOKEN}")
    curl -fsSL "${auth[@]}" "https://api.github.com/repos/${repo}/releases?per_page=50" \
        | grep -o '"tag_name": *"[^"]*"' | sed 's/.*"\([^"]*\)"$/\1/' \
        | grep "^${prefix}v[0-9]" | grep -v -- '-\(alpha\|beta\|rc\)' | head -1
}

# verify_sha256 <fichier> <somme attendue>
verify_sha256() {
    local got
    got="$(sha256sum "$1" | awk '{print $1}')"
    if [ "${got}" != "$2" ]; then
        echo "Somme SHA-256 invalide pour $1 (attendu $2, obtenu ${got})" >&2
        exit 1
    fi
}

# --- Prérequis système (installés via apt, hors du périmètre de ce script) ----

missing_apt=()
for tool in git make curl tar gcc; do
    command -v "${tool}" >/dev/null 2>&1 || missing_apt+=("${tool}")
done
if [ ${#missing_apt[@]} -gt 0 ]; then
    log absent "${missing_apt[*]} — installez-les : sudo apt install -y git make curl tar build-essential"
    # gcc n'est requis que pour `go test -race` ; les autres sont bloquants.
    for t in "${missing_apt[@]}"; do [ "${t}" != gcc ] && exit 1; done
fi

# --- Go ------------------------------------------------------------------

if needs_install go; then
    version="${GO_VERSION:-$(curl -fsSL 'https://go.dev/VERSION?m=text' | head -1)}"
    archive="${version}.${OS}-${ARCH}.tar.gz"
    log install "${version} -> ${GO_ROOT}"
    curl -fsSLo "${TMP}/${archive}" "https://go.dev/dl/${archive}"
    sum="$(curl -fsSL 'https://go.dev/dl/?mode=json&include=all' \
        | grep -A4 "\"filename\": \"${archive}\"" | grep -o '"sha256": "[a-f0-9]*"' | cut -d'"' -f4)"
    verify_sha256 "${TMP}/${archive}" "${sum}"
    rm -rf "${GO_ROOT}"
    tar -xzf "${TMP}/${archive}" -C "${PREFIX}"
fi

# --- kubectl -------------------------------------------------------------

if needs_install kubectl; then
    version="${KUBECTL_VERSION:-$(curl -fsSL https://dl.k8s.io/release/stable.txt)}"
    log install "kubectl ${version}"
    url="https://dl.k8s.io/release/${version}/bin/${OS}/${ARCH}/kubectl"
    curl -fsSLo "${TMP}/kubectl" "${url}"
    verify_sha256 "${TMP}/kubectl" "$(curl -fsSL "${url}.sha256")"
    install -m 0755 "${TMP}/kubectl" "${BIN_DIR}/kubectl"
fi

# --- Helm ----------------------------------------------------------------

if needs_install helm; then
    version="${HELM_VERSION:-$(gh_latest helm/helm)}"
    archive="helm-${version}-${OS}-${ARCH}.tar.gz"
    log install "helm ${version}"
    curl -fsSLo "${TMP}/${archive}" "https://get.helm.sh/${archive}"
    verify_sha256 "${TMP}/${archive}" "$(curl -fsSL "https://get.helm.sh/${archive}.sha256sum" | awk '{print $1}')"
    tar -xzf "${TMP}/${archive}" -C "${TMP}"
    install -m 0755 "${TMP}/${OS}-${ARCH}/helm" "${BIN_DIR}/helm"
fi

# --- kind (cluster local) --------------------------------------------------

if needs_install kind; then
    version="${KIND_VERSION:-$(gh_latest kubernetes-sigs/kind)}"
    log install "kind ${version}"
    url="https://github.com/kubernetes-sigs/kind/releases/download/${version}/kind-${OS}-${ARCH}"
    curl -fsSLo "${TMP}/kind" "${url}"
    verify_sha256 "${TMP}/kind" "$(curl -fsSL "${url}.sha256sum" | awk '{print $1}')"
    install -m 0755 "${TMP}/kind" "${BIN_DIR}/kind"
fi

# --- kustomize -----------------------------------------------------------

if needs_install kustomize; then
    tag="${KUSTOMIZE_VERSION:+kustomize/${KUSTOMIZE_VERSION}}"
    tag="${tag:-$(gh_latest kubernetes-sigs/kustomize kustomize/)}"
    version="${tag#kustomize/}"
    log install "kustomize ${version}"
    curl -fsSL "https://github.com/kubernetes-sigs/kustomize/releases/download/kustomize%2F${version}/kustomize_${version}_${OS}_${ARCH}.tar.gz" \
        | tar -xz -C "${BIN_DIR}" kustomize
fi

# --- Kubebuilder (scaffolding Go) -------------------------------------------

if needs_install kubebuilder; then
    version="${KUBEBUILDER_VERSION:-$(gh_latest kubernetes-sigs/kubebuilder)}"
    log install "kubebuilder ${version}"
    curl -fsSLo "${TMP}/kubebuilder" \
        "https://github.com/kubernetes-sigs/kubebuilder/releases/download/${version}/kubebuilder_${OS}_${ARCH}"
    install -m 0755 "${TMP}/kubebuilder" "${BIN_DIR}/kubebuilder"
fi

# --- Operator SDK (scaffolding Go/Helm/Ansible, bundles OLM) -----------------

if needs_install operator-sdk; then
    version="${OPERATOR_SDK_VERSION:-$(gh_latest operator-framework/operator-sdk)}"
    log install "operator-sdk ${version}"
    curl -fsSLo "${TMP}/operator-sdk" \
        "https://github.com/operator-framework/operator-sdk/releases/download/${version}/operator-sdk_${OS}_${ARCH}"
    install -m 0755 "${TMP}/operator-sdk" "${BIN_DIR}/operator-sdk"
fi

# --- opm (catalogues OLM) ---------------------------------------------------

if needs_install opm; then
    version="${OPM_VERSION:-$(gh_latest operator-framework/operator-registry)}"
    log install "opm ${version}"
    curl -fsSLo "${TMP}/opm" \
        "https://github.com/operator-framework/operator-registry/releases/download/${version}/${OS}-${ARCH}-opm"
    install -m 0755 "${TMP}/opm" "${BIN_DIR}/opm"
fi

# --- golangci-lint (lint Go) ------------------------------------------------

if needs_install golangci-lint; then
    version="${GOLANGCI_LINT_VERSION:-$(gh_latest golangci/golangci-lint)}"
    dir="golangci-lint-${version#v}-${OS}-${ARCH}"
    log install "golangci-lint ${version}"
    curl -fsSL "https://github.com/golangci/golangci-lint/releases/download/${version}/${dir}.tar.gz" \
        | tar -xz -C "${TMP}"
    install -m 0755 "${TMP}/${dir}/golangci-lint" "${BIN_DIR}/golangci-lint"
fi

# --- Outils installés via `go install` ---------------------------------------
# Chaque projet épingle ses propres versions dans son Makefile (./bin) : les
# versions globales ci-dessous servent hors projet et pour le scaffolding.

if needs_install controller-gen; then
    log install "controller-gen ${CONTROLLER_GEN_VERSION:-latest}"
    GOBIN="${BIN_DIR}" go install "sigs.k8s.io/controller-tools/cmd/controller-gen@${CONTROLLER_GEN_VERSION:-latest}"
fi

if needs_install setup-envtest; then
    log install "setup-envtest ${SETUP_ENVTEST_VERSION:-latest}"
    GOBIN="${BIN_DIR}" go install "sigs.k8s.io/controller-runtime/tools/setup-envtest@${SETUP_ENVTEST_VERSION:-latest}"
fi

# --- Docker (non installé par ce script) --------------------------------------

if command -v docker >/dev/null 2>&1 && docker info >/dev/null 2>&1; then
    log ok docker
else
    log absent "docker — activez l'intégration WSL de Docker Desktop (Settings > Resources > WSL integration)"
fi

# --- PATH ----------------------------------------------------------------

path_line='export PATH="$HOME/.local/go/bin:$HOME/go/bin:$HOME/.local/bin:$PATH"'
if ! grep -qF "${path_line}" "${HOME}/.bashrc" 2>/dev/null; then
    echo "${path_line}" >> "${HOME}/.bashrc"
    log path "ajouté à ~/.bashrc"
fi

# --- Récapitulatif ---------------------------------------------------------

echo
echo "Versions disponibles :"
for tool in go kubectl helm kind kustomize kubebuilder operator-sdk opm golangci-lint controller-gen setup-envtest; do
    path="$(command -v "${tool}" 2>/dev/null || true)"
    printf '  %-15s %s\n' "${tool}" "${path:-ABSENT}"
done
go version

echo
echo "Terminé. Rechargez le shell : source ~/.bashrc"
