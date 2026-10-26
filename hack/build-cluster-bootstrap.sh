#!/usr/bin/env bash
# Provisions the cluster-side prerequisites for a remote build: the `build`
# namespace and the docker.io push secret the Job mounts. Idempotent — running
# it twice exits 0 and leaves exactly one namespace and one secret.
#
# Called by hand before the first `make build BUILD_TARGET=<cluster>` against a
# cluster. It deliberately creates no Job: jobs are per-build, rendered by
# hack/build-cluster.sh, which replaces a finished Job and refuses a running one.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEMPLATE="${TEMPLATE:-${SCRIPT_DIR}/dockerconfig.json.tpl}"

BUILD_TARGET="${1:-}"
# The Makefile exposes this knob as BUILD_NAMESPACE and passes it to
# hack/build-cluster.sh as NAMESPACE. Accept either spelling here, or a
# non-default BUILD_NAMESPACE provisions the secret into `build` while the Job
# lands in the requested namespace and fails on a missing secret.
NAMESPACE="${BUILD_NAMESPACE:-${NAMESPACE:-build}}"
BUILD_PUSH_SECRET="${BUILD_PUSH_SECRET:-buildkit-push}"

die() {
	echo "build-cluster-bootstrap: $*" >&2
	exit 1
}

[ -n "${BUILD_TARGET}" ] || die "usage: $(basename "${BASH_SOURCE[0]}") <sun|nukedev|nukeprod|kubectl-context>"
[ -f "${TEMPLATE}" ] || die "template not found: ${TEMPLATE}"

# teamvault-cli's own config, which is NOT the file `make apply` reads: Makefile.k8s
# passes ${TEAMVAULT}, defaulted in Makefile.variables to ~/.teamvault.json, and
# teamvault-config-parser consumes that. This script needs the CLI instead (see the
# note below), so it needs the CLI's config. Deliberately a different variable name:
# a TEAMVAULT exported by the make environment would otherwise silently point this
# at the parser's file. Resolution follows the order teamvault-cli documents —
# $TEAMVAULT_CONFIG, then ~/.config/teamvault-cli/config.json, then ~/.teamvault.json
# — so an operator who configured the CLI that way is not told its config is missing.
if [ -z "${TEAMVAULT_CLI_CONFIG:-}" ]; then
	for candidate in "${TEAMVAULT_CONFIG:-}" \
		"${HOME}/.config/teamvault-cli/config.json" \
		"${HOME}/.teamvault.json"; do
		[ -n "${candidate}" ] || continue
		if [ -f "${candidate}" ]; then
			TEAMVAULT_CLI_CONFIG="${candidate}"
			break
		fi
	done
fi
[ -n "${TEAMVAULT_CLI_CONFIG:-}" ] ||
	die "no teamvault-cli config found (set TEAMVAULT_CLI_CONFIG, or TEAMVAULT_CONFIG as the CLI reads it)"
# An explicit override short-circuits the loop above, so the resolved path is tested
# here. The hand-typed value is exactly the case a human gets wrong, and it deserves
# the named diagnostic rather than whatever teamvault-cli prints.
[ -f "${TEAMVAULT_CLI_CONFIG}" ] ||
	die "teamvault-cli config not found: ${TEAMVAULT_CLI_CONFIG}"
command -v teamvault-cli >/dev/null 2>&1 ||
	die "teamvault-cli not found on PATH; it resolves the lookup key in ${TEMPLATE}"

# Shared with hack/build-cluster.sh so the alias mapping cannot drift between
# provisioning and building.
source "${SCRIPT_DIR}/cluster-alias.sh"

# The same check hack/build-cluster.sh makes. Without it, a resolved alias whose
# kubeconfig file is absent surfaces as an opaque `context "default" does not
# exist` from the first kubectl call instead of naming the missing file.
if [ -n "${KUBECONFIG_PATH}" ] && [ ! -f "${KUBECONFIG_PATH}" ]; then
	die "BUILD_TARGET '${BUILD_TARGET}' needs the kubeconfig ${KUBECONFIG_PATH}, which does not exist"
fi

# The rendered credential lives in a 600 temp file for the length of one
# `create secret --dry-run` and is removed on every exit path. It is never
# echoed, never logged, and never written into the repo.
RENDERED="$(mktemp)"
chmod 600 "${RENDERED}"
trap 'rm -f "${RENDERED}"' EXIT INT TERM

echo "build-cluster-bootstrap: cluster ${BUILD_TARGET} (kubectl context ${KUBE_CONTEXT})"

kc create namespace "${NAMESPACE}" --dry-run=client -o yaml | kc apply -f -
echo "build-cluster-bootstrap: namespace ${NAMESPACE} present"

# Both the TeamVault item and the registry host come from the template, so the two
# cannot disagree: a template repointed at another registry renders the credential
# under that registry's auths key. This does not tie the secret to the registry the
# Job actually pushes to (DOCKER_REGISTRY) — keep those in step when either changes.
TEAMVAULT_KEY="$(sed -nE 's/.*"([A-Za-z0-9]{6,12})" \| teamvaultUser.*/\1/p' "${TEMPLATE}" | head -1)"
[ -n "${TEAMVAULT_KEY}" ] || die "no teamvaultUser lookup key found in ${TEMPLATE}"
# `|| true` is load-bearing: grep exits 1 when nothing matches, pipefail propagates
# that through `head`, and `set -e` would kill the script at this assignment — before
# the guard below could name the problem. Only an `https://`-prefixed auths key is
# supported: a bare-host key (registry.example.com) is valid dockerconfigjson but this
# extraction cannot read it, so it fails here rather than pushing under the wrong key.
AUTHS_KEY="$(grep -oE 'https?://[^"]+' "${TEMPLATE}" | head -1 || true)"
[ -n "${AUTHS_KEY}" ] || die "no https://-prefixed auths key found in ${TEMPLATE} (a bare host is valid dockerconfigjson, but this extraction cannot read it)"

# teamvault-cli, not teamvault-config-parser: the parser takes its password from
# the config file, which holds only url and user, so it authenticates unprivileged
# and gets 403 on this item. The CLI reads the login from the macOS Keychain.
TV_USER="$(teamvault-cli username --teamvault-config "${TEAMVAULT_CLI_CONFIG}" --teamvault-key="${TEAMVAULT_KEY}")"
TV_PASS="$(teamvault-cli password --teamvault-config "${TEAMVAULT_CLI_CONFIG}" --teamvault-key="${TEAMVAULT_KEY}")"
[ -n "${TV_USER}" ] && [ -n "${TV_PASS}" ] || die "teamvault returned an empty credential for key ${TEAMVAULT_KEY}"

# jq encodes the values, so a credential containing a quote or a backslash cannot
# produce malformed JSON or inject a second auths entry — printf '%s' would do both.
# The credential reaches jq through the environment, never argv: `ps` shows a
# process's arguments to every local user, and the temp file's promise above covers
# echoing and logging but not that third channel. Reading them back needs jq >= 1.6,
# which provides env/0; older builds fail with jq's own `env/0 is not defined`.
command -v jq >/dev/null 2>&1 || die "jq not found on PATH; it JSON-encodes the credential"
TV_USER="${TV_USER}" TV_PASS="${TV_PASS}" jq -n --arg key "${AUTHS_KEY}" \
	'{auths:{($key):{username:env.TV_USER,password:env.TV_PASS}}}' >"${RENDERED}"
unset TV_USER TV_PASS
[ -s "${RENDERED}" ] || die "failed to render ${TEMPLATE}"

# --server-side: client-side apply records the whole applied manifest — including
# .data.dockerconfigjson — in the kubectl.kubernetes.io/last-applied-configuration
# annotation, where it outlives a rotation and is exported by tooling and backups that
# redact .data but not annotations. --force-conflicts is required on any cluster
# provisioned by the previous client-side version, whose field manager owns the field;
# and SSA does not remove the annotation that version wrote, so it is deleted here.
kc create secret generic "${BUILD_PUSH_SECRET}" \
	-n "${NAMESPACE}" \
	--type=kubernetes.io/dockerconfigjson \
	--from-file=.dockerconfigjson="${RENDERED}" \
	--dry-run=client -o yaml | kc apply --server-side --force-conflicts -f -
kc annotate secret "${BUILD_PUSH_SECRET}" -n "${NAMESPACE}" \
	kubectl.kubernetes.io/last-applied-configuration- --overwrite >/dev/null
echo "build-cluster-bootstrap: secret ${NAMESPACE}/${BUILD_PUSH_SECRET} present"

echo "build-cluster-bootstrap: ready — make build BUILD_TARGET=${BUILD_TARGET}"
