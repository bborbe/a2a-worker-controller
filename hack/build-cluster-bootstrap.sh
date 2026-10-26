#!/usr/bin/env bash
# Provisions the cluster-side prerequisites for a remote build: the `build`
# namespace and the docker.io push secret the Job mounts. Idempotent — running
# it twice exits 0 and leaves exactly one namespace and one secret.
#
# Called by hand before the first `make build BUILD_TARGET=<cluster>` against a
# cluster. It deliberately creates no Job: jobs are per-build, rendered by
# hack/build-cluster.sh, which refuses to run when one already exists.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEMPLATE="${TEMPLATE:-${SCRIPT_DIR}/dockerconfig.json.tpl}"

BUILD_TARGET="${1:-}"
NAMESPACE="${NAMESPACE:-build}"
BUILD_PUSH_SECRET="${BUILD_PUSH_SECRET:-buildkit-push}"

die() {
	echo "build-cluster-bootstrap: $*" >&2
	exit 1
}

[ -n "${BUILD_TARGET}" ] || die "usage: $(basename "${BASH_SOURCE[0]}") <sun|nukedev|nukeprod|kubectl-context>"
[ -f "${TEMPLATE}" ] || die "template not found: ${TEMPLATE}"

# The same -teamvault-config value `make apply` takes (Makefile.k8s). Defaults to
# the config the repo's own .envrc uses, so an operator who has teamvault-cli set
# up does not have to export anything.
TEAMVAULT="${TEAMVAULT:-${HOME}/.config/teamvault-cli/config.json}"
[ -f "${TEAMVAULT}" ] || die "teamvault config not found: ${TEAMVAULT} (set TEAMVAULT to override)"
command -v teamvault-cli >/dev/null 2>&1 ||
	die "teamvault-cli not found on PATH; it resolves the lookup key in ${TEMPLATE}"

# Shared with hack/build-cluster.sh so the alias mapping cannot drift between
# provisioning and building.
source "${SCRIPT_DIR}/cluster-alias.sh"

# The rendered credential lives in a 600 temp file for the length of one
# `create secret --dry-run` and is removed on every exit path. It is never
# echoed, never logged, and never written into the repo.
RENDERED="$(mktemp)"
chmod 600 "${RENDERED}"
trap 'rm -f "${RENDERED}"' EXIT INT TERM

echo "build-cluster-bootstrap: cluster ${BUILD_TARGET} (kubectl context ${KUBE_CONTEXT})"

kc create namespace "${NAMESPACE}" --dry-run=client -o yaml | kc apply -f -
echo "build-cluster-bootstrap: namespace ${NAMESPACE} present"

# The lookup key comes from the template, so the template stays the single
# declaration of which TeamVault item and which registry this secret is for.
TEAMVAULT_KEY="$(sed -nE 's/.*"([A-Za-z0-9]{6,12})" \| teamvaultUser.*/\1/p' "${TEMPLATE}" | head -1)"
[ -n "${TEAMVAULT_KEY}" ] || die "no teamvaultUser lookup key found in ${TEMPLATE}"

# teamvault-cli, not teamvault-config-parser: the parser takes its password from
# the config file, which holds only url and user, so it authenticates unprivileged
# and gets 403 on this item. The CLI reads the login from the macOS Keychain.
TV_USER="$(teamvault-cli username --teamvault-config "${TEAMVAULT}" --teamvault-key="${TEAMVAULT_KEY}")"
TV_PASS="$(teamvault-cli password --teamvault-config "${TEAMVAULT}" --teamvault-key="${TEAMVAULT_KEY}")"
[ -n "${TV_USER}" ] && [ -n "${TV_PASS}" ] || die "teamvault returned an empty credential for key ${TEAMVAULT_KEY}"

# docker.io's canonical auths key, per the template.
printf '{"auths":{"https://index.docker.io/v1/":{"username":"%s","password":"%s"}}}' \
	"${TV_USER}" "${TV_PASS}" >"${RENDERED}"
unset TV_USER TV_PASS
[ -s "${RENDERED}" ] || die "failed to render ${TEMPLATE}"

kc create secret generic "${BUILD_PUSH_SECRET}" \
	-n "${NAMESPACE}" \
	--type=kubernetes.io/dockerconfigjson \
	--from-file=.dockerconfigjson="${RENDERED}" \
	--dry-run=client -o yaml | kc apply -f -
echo "build-cluster-bootstrap: secret ${NAMESPACE}/${BUILD_PUSH_SECRET} present"

echo "build-cluster-bootstrap: ready — make build BUILD_TARGET=${BUILD_TARGET}"
