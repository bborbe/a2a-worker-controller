#!/usr/bin/env bash
# Renders hack/build-job.yaml and runs it in the target cluster.
#
# Called from `make build BUILD_TARGET=<sun|nukedev|nukeprod|kubectl-context>`
# (Makefile.docker) with BUILD_TARGET, NAMESPACE, REPO_URL, REF, TAG,
# DOCKERFILE, IMAGE, DOCKER_REGISTRY, PUSH, PUSH_SECRET and TIMEOUT in the
# environment. Not intended to be run by hand.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEMPLATE="${TEMPLATE:-${SCRIPT_DIR}/build-job.yaml}"

NAMESPACE="${NAMESPACE:-build}"
PUSH="${PUSH:-true}"
TIMEOUT="${TIMEOUT:-3600}"
DOCKERFILE="${DOCKERFILE:-Dockerfile}"

die() {
	echo "build-cluster: $*" >&2
	exit 1
}

require() {
	local name="$1"
	[ -n "${!name:-}" ] || die "${name} is required; pass it on the make command line"
}

# BUILD_TARGET is resolved to a kubeconfig file and context by cluster-alias.sh,
# which also defines the kc() helper every kubectl call goes through. It is
# shared with hack/build-cluster-bootstrap.sh so the alias mapping cannot drift
# between provisioning and building.
source "${SCRIPT_DIR}/cluster-alias.sh"

# The equivalent invocation for messages the operator has to retype.
KUBE_HINT="kubectl --context ${KUBE_CONTEXT}"
[ -z "${KUBECONFIG_PATH}" ] || KUBE_HINT="KUBECONFIG=${KUBECONFIG_PATH} ${KUBE_HINT}"

require BUILD_TARGET
require REPO_URL
require REF
require TAG
require IMAGE
require DOCKER_REGISTRY
require PUSH_SECRET

# Values are substituted into the manifest with sed and land in shell scripts
# inside it, so each one is restricted to a whitelist before rendering. A value
# outside the whitelist is rejected, never escaped.
#
# `case` decides the two shapes a grep cannot see. grep reads line by line, so a
# value carrying a newline passed as soon as one of its lines matched; and a
# leading `-` or `..` sits inside every whitelist below even though REF reaches
# `git fetch`, which would read `--upload-pack=…` as an option.
check_value() {
	local name="$1" value="$2" pattern="$3"
	case "${value}" in
	"") die "${name} is empty" ;;
	-* | ..*) die "${name} '${value}' starts with '-' or '..'" ;;
	*$'\n'*) die "${name} contains a newline" ;;
	esac
	printf '%s' "${value}" | grep -qE "${pattern}" ||
		die "${name} '${value}' is outside ${pattern}"
}

check_value REF "${REF}" '^[A-Za-z0-9._/-]+$'
check_value TAG "${TAG}" '^[A-Za-z0-9._-]+$'
check_value IMAGE "${IMAGE}" '^[A-Za-z0-9._/:-]+$'
check_value DOCKER_REGISTRY "${DOCKER_REGISTRY}" '^[A-Za-z0-9._:/-]+$'
check_value REPO_URL "${REPO_URL}" '^[A-Za-z0-9._/:@-]+$'
# A URL carrying userinfo is rendered into the Job spec, where anyone who can
# read the namespace recovers it. SCP-style git@host:path has no `://`, so it is
# unaffected; a private repository should mount a credential instead.
case "${REPO_URL}" in
*://*@*) die "REPO_URL must not embed credentials; use a credential the Job can mount" ;;
esac
check_value DOCKERFILE "${DOCKERFILE}" '^[A-Za-z0-9._/-]+$'
check_value PUSH_SECRET "${PUSH_SECRET}" '^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$'
check_value NAMESPACE "${NAMESPACE}" '^[a-z0-9]([-a-z0-9]*[a-z0-9])?$'
# Through check_value, not a bare printf|grep: that form accepts a value carrying an
# embedded newline, and this value is rendered into activeDeadlineSeconds.
check_value TIMEOUT "${TIMEOUT}" '^[0-9]+$'
case "${PUSH}" in
true | false) ;;
*) die "PUSH '${PUSH}' must be true or false" ;;
esac

if [ -n "${KUBECONFIG_PATH}" ] && [ ! -f "${KUBECONFIG_PATH}" ]; then
	die "BUILD_TARGET '${BUILD_TARGET}' needs the kubeconfig ${KUBECONFIG_PATH}, which does not exist"
fi

# The accepted targets are the contexts the resolved kubeconfig knows about, so
# an unknown target fails here with the list instead of at apply time. The
# aliases are listed too: they are valid inputs without being contexts.
contexts="$(kc config get-contexts -o name 2>/dev/null || true)"
printf '%s\n' "${contexts}" | grep -qxF "${KUBE_CONTEXT}" ||
	die "unknown BUILD_TARGET '${BUILD_TARGET}'; accepted values: sun nukedev nukeprod $(printf '%s' "${contexts}" | tr '\n' ' ')"

# The build clock is the built commit's timestamp. It is rendered literally so
# the Job states the epoch it will use; the init container re-derives it from
# the cloned commit and the build container refuses to run on a mismatch.
commit_epoch="$(git log -1 --format=%ct "${REF}" 2>/dev/null || true)"
# An ambient SOURCE_DATE_EPOCH stays honoured as an escape hatch, but only when it
# agrees with the commit. The Job states the epoch it will use and the build container
# refuses on a mismatch, so a stale exported value would fail *inside* the cluster with
# "run git fetch and retry" — advice that cannot fix an exported variable.
if [ -n "${SOURCE_DATE_EPOCH:-}" ] && [ -n "${commit_epoch}" ] && [ "${SOURCE_DATE_EPOCH}" != "${commit_epoch}" ]; then
	die "SOURCE_DATE_EPOCH ${SOURCE_DATE_EPOCH} disagrees with ${REF}'s commit epoch ${commit_epoch}; unset it, or set it to that value"
fi
[ -n "${SOURCE_DATE_EPOCH:-}" ] || SOURCE_DATE_EPOCH="${commit_epoch}"
# An empty value means REF did not resolve locally; anything else must be a plain
# number. Through check_value rather than printf|grep, which matches on the first
# line and would let a multi-line exported value reach the manifest with an
# embedded newline — the same hole closed for TIMEOUT.
[ -n "${SOURCE_DATE_EPOCH}" ] ||
	die "cannot resolve REF '${REF}' to a commit in the local repository; run git fetch and retry"
check_value SOURCE_DATE_EPOCH "${SOURCE_DATE_EPOCH}" '^[0-9]+$'

# BUILD_DATE is derived from the commit epoch rather than read from the clock, so
# two builds of one commit stamp the same date. BSD and GNU date disagree on the
# flag that reads an epoch, and this runs on the operator's Mac.
if ! BUILD_DATE="$(date -u -r "${SOURCE_DATE_EPOCH}" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null)"; then
	BUILD_DATE="$(date -u -d "@${SOURCE_DATE_EPOCH}" +%Y-%m-%dT%H:%M:%SZ)"
fi

JOB_NAME="build-$(printf '%s' "${TAG}" | tr '[:upper:]' '[:lower:]' | sed -e 's/[^a-z0-9.-]/-/g')"
# A Job name is a DNS label: truncation can leave a trailing '-' or '.', which
# the API server rejects as an invalid name.
JOB_NAME="$(printf '%s' "${JOB_NAME:0:63}" | sed -e 's/[.-]*$//')"
[ -n "${JOB_NAME}" ] || die "cannot derive a Job name from TAG '${TAG}'"

rendered="$(mktemp)"
trap 'rm -f "${rendered}"' EXIT

sed \
	-e "s|__JOB_NAME__|${JOB_NAME}|g" \
	-e "s|__NAMESPACE__|${NAMESPACE}|g" \
	-e "s|__REPO_URL__|${REPO_URL}|g" \
	-e "s|__REF__|${REF}|g" \
	-e "s|__TAG__|${TAG}|g" \
	-e "s|__DOCKERFILE__|${DOCKERFILE}|g" \
	-e "s|__IMAGE__|${IMAGE}|g" \
	-e "s|__PUSH__|${PUSH}|g" \
	-e "s|__PUSH_SECRET__|${PUSH_SECRET}|g" \
	-e "s|__DEADLINE_SECONDS__|${TIMEOUT}|g" \
	-e "s|__SOURCE_DATE_EPOCH__|${SOURCE_DATE_EPOCH}|g" \
	-e "s|__BUILD_DATE__|${BUILD_DATE}|g" \
	-e "s|__DOCKER_REGISTRY__|${DOCKER_REGISTRY}|g" \
	"${TEMPLATE}" >"${rendered}"

if kc -n "${NAMESPACE}" get job "${JOB_NAME}" --ignore-not-found -o name | grep -q .; then
	# A finished Job keeps its name until ttlSecondsAfterFinished elapses (24h), so
	# refusing on any collision makes the second build of a tag impossible —
	# including the retry after a failed one, which is the ordinary
	# build-fix-retry loop this feature exists to serve. Only a still-running Job
	# is a real conflict: two builds of one tag racing. Replace a terminal Job;
	# refuse a live one.
	# The read's exit status is kept: `2>/dev/null || true` would collapse a transient
	# API error into an empty string, which the case below would then report as
	# "still running" — the same conflation the wait loop further down avoids.
	if ! terminal="$(kc -n "${NAMESPACE}" get job "${JOB_NAME}" \
		-o jsonpath='{range .status.conditions[?(@.status=="True")]}{.type}{"\n"}{end}')"; then
		die "cannot read job ${NAMESPACE}/${JOB_NAME} to decide whether it is finished; retry, or delete it with: ${KUBE_HINT} -n ${NAMESPACE} delete job ${JOB_NAME}"
	fi
	case "${terminal}" in
	# SuccessCriteriaMet and FailureTarget are the delayed-terminal-condition markers:
	# the controller sets them seconds before Complete/Failed, and a retry landing in
	# that window would otherwise be refused as "still running".
	*Complete* | *Failed* | *SuccessCriteriaMet* | *FailureTarget*)
		echo "build-cluster: replacing finished job ${NAMESPACE}/${JOB_NAME}"
		# Foreground cascade, so the old Job's pods are gone before the new Job's logs
		# are selected by the job-name label — otherwise a failed retry interleaves the
		# previous run's lines. --ignore-not-found covers the TTL elapsing between the
		# read above and this delete: a missing Job is the desired end state.
		kc -n "${NAMESPACE}" delete job "${JOB_NAME}" \
			--cascade=foreground --wait=true --ignore-not-found >/dev/null
		;;
	*)
		die "job ${NAMESPACE}/${JOB_NAME} is still running; wait for it or delete it with: ${KUBE_HINT} -n ${NAMESPACE} delete job ${JOB_NAME}"
		;;
	esac
fi

if ! kc apply -f "${rendered}"; then
	echo "build-cluster: job ${NAMESPACE}/${JOB_NAME} was not accepted; events:" >&2
	kc -n "${NAMESPACE}" get events \
		--field-selector "involvedObject.name=${JOB_NAME}" \
		--sort-by=.lastTimestamp >&2 || true
	die "could not create job ${NAMESPACE}/${JOB_NAME}"
fi

# 10# forces base 10: a leading zero would otherwise be read as octal, so
# BUILD_TIMEOUT=0600 would mean 384 seconds and 0800 would abort outright.
deadline=$(( $(date +%s) + 10#${TIMEOUT} + 120 ))
outcome=""
while [ -z "${outcome}" ]; do
	# One read, and its exit status is kept. `|| true` on each counter made a
	# deleted Job or a transient API error indistinguishable from a running one,
	# so the loop waited out the whole TIMEOUT instead of failing fast.
	if ! state="$(kc -n "${NAMESPACE}" get job "${JOB_NAME}" \
		--ignore-not-found -o jsonpath='{.status.succeeded} {.status.failed}' 2>/dev/null)"; then
		outcome=unreachable
		break
	fi
	if [ -z "${state}" ]; then
		outcome=gone
		break
	fi
	succeeded="${state%% *}"
	failed="${state##* }"
	if [ "${succeeded:-0}" -ge 1 ]; then
		outcome=succeeded
	elif [ "${failed:-0}" -ge 1 ]; then
		outcome=failed
	elif [ "$(date +%s)" -ge "${deadline}" ]; then
		outcome=timeout
	else
		sleep 5
	fi
done

logs() {
	kc -n "${NAMESPACE}" logs \
		-l "job-name=${JOB_NAME}" --all-containers --prefix "$@" 2>/dev/null || true
}

if [ "${outcome}" = succeeded ]; then
	logs --tail=-1 | grep -E 'pushing manifest for|containerimage\.digest' || true
	echo "build-cluster: job ${NAMESPACE}/${JOB_NAME} succeeded"
	exit 0
fi

echo "build-cluster: job ${NAMESPACE}/${JOB_NAME} ${outcome}; last log lines:" >&2
logs --tail=100 >&2
case "${outcome}" in
timeout)
	die "job ${NAMESPACE}/${JOB_NAME} did not finish within ${TIMEOUT}s (+120s grace)"
	;;
gone)
	die "job ${NAMESPACE}/${JOB_NAME} disappeared while it was being watched; it was deleted, or ttlSecondsAfterFinished elapsed"
	;;
unreachable)
	die "cannot read job ${NAMESPACE}/${JOB_NAME}; kubectl failed, so its outcome is unknown — check ${KUBE_HINT} -n ${NAMESPACE} get job ${JOB_NAME}"
	;;
esac
die "job ${NAMESPACE}/${JOB_NAME} failed"
