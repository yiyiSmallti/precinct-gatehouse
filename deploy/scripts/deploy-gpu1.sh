#!/usr/bin/env bash
set -euo pipefail

SSH_HOST="${GATEHOUSE_SSH_HOST:-gpu1}"
REMOTE_HOME="${GATEHOUSE_REMOTE_HOME:-/AIOS/sglang-kunlun-v0522-1017/deploy-dsv41-1017/gateway/gatehouse}"
GH_REPO="${GATEHOUSE_GH_REPO:-yiyiSmallti/precinct-gatehouse}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
REPO_ROOT="$(cd "${DEPLOY_ROOT}/.." && pwd)"

SSH_OPTS=(-o BatchMode=yes -o ConnectTimeout=10)
LOCAL_TMP=""
REMOTE_TMP=""

cleanup() {
  if [ -n "${LOCAL_TMP}" ] && [ -d "${LOCAL_TMP}" ]; then
    rm -rf "${LOCAL_TMP}"
  fi
  if [ -n "${REMOTE_TMP}" ]; then
    ssh "${SSH_OPTS[@]}" "${SSH_HOST}" "rm -rf '${REMOTE_TMP}'" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

log() {
  printf '[deploy-gpu1] %s\n' "$*"
}

die() {
  printf '[deploy-gpu1] ERROR: %s\n' "$*" >&2
  exit 1
}

sha256_file() {
  local path="$1"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$path" | awk '{print tolower($1)}'
  else
    shasum -a 256 "$path" | awk '{print tolower($1)}'
  fi
}

validate_version() {
  local ver="$1"
  if [[ ! "${ver}" =~ ^[A-Za-z0-9._-]+$ ]] || [ "${ver}" = "." ] || [ "${ver}" = ".." ]; then
    die "Invalid version '${ver}'. Allowed characters: A-Z a-z 0-9 . _ -"
  fi
}

upload_and_install() {
  local ver="$1"
  local bin_path="$2"
  local bin_sha="$3"
  local meta_path="${4:-}"

  REMOTE_TMP="$(ssh "${SSH_OPTS[@]}" "${SSH_HOST}" "mktemp -d '/tmp/gatehouse-deploy-XXXXXX'")"
  log "Uploading ${ver} to ${SSH_HOST}:${REMOTE_TMP}..."
  if [ -n "${meta_path}" ] && [ -f "${meta_path}" ]; then
    scp -q "${SSH_OPTS[@]}" "${bin_path}" "${meta_path}" "${SSH_HOST}:${REMOTE_TMP}/"
    ssh "${SSH_OPTS[@]}" "${SSH_HOST}" \
      "'${REMOTE_HOME}/scripts/apply-release.sh' install '${ver}' '${REMOTE_TMP}/gatehouse' --sha256 '${bin_sha}' --meta '${REMOTE_TMP}/RELEASE_META'"
  else
    scp -q "${SSH_OPTS[@]}" "${bin_path}" "${SSH_HOST}:${REMOTE_TMP}/gatehouse"
    ssh "${SSH_OPTS[@]}" "${SSH_HOST}" \
      "'${REMOTE_HOME}/scripts/apply-release.sh' install '${ver}' '${REMOTE_TMP}/gatehouse' --sha256 '${bin_sha}'"
  fi
}

cmd_pull() {
  local tag="${1:-latest}"
  log "Triggering mirror pull on ${SSH_HOST} (tag: ${tag})..."
  ssh "${SSH_OPTS[@]}" "${SSH_HOST}" "'${REMOTE_HOME}/scripts/pull-and-deploy.sh' '${tag}'"
}

cmd_check_mirror() {
  local tag="${1:-latest}"
  ssh "${SSH_OPTS[@]}" "${SSH_HOST}" "'${REMOTE_HOME}/scripts/pull-and-deploy.sh' --check '${tag}'"
}

cmd_dry_run_mirror() {
  local tag="${1:-latest}"
  ssh "${SSH_OPTS[@]}" "${SSH_HOST}" "'${REMOTE_HOME}/scripts/pull-and-deploy.sh' --dry-run '${tag}'"
}

cmd_release() {
  command -v gh >/dev/null 2>&1 || die "GitHub CLI (gh) is required for 'release' subcommand"
  local ver="${1:-}"
  if [ -z "${ver}" ] || [ "${ver}" = "--latest" ]; then
    ver="$(gh release view --repo "${GH_REPO}" --json tagName --jq '.tagName' 2>/dev/null || true)"
    if [ -z "${ver}" ]; then
      ver="$(gh release list --repo "${GH_REPO}" --limit 1 --json tagName --jq '.[0].tagName' 2>/dev/null || true)"
    fi
    [ -n "${ver}" ] || die "No GitHub Release found in ${GH_REPO}"
  fi
  validate_version "${ver}"

  LOCAL_TMP="$(mktemp -d)"
  local tarball="gatehouse_${ver}_linux_amd64.tar.gz"
  log "Downloading ${ver} from GitHub Release (${GH_REPO})..."
  gh release download "${ver}" --repo "${GH_REPO}" -D "${LOCAL_TMP}" -p "${tarball}" -p "SHA256SUMS"

  local expected_tar_sha actual_tar_sha
  expected_tar_sha="$(awk -v f="${tarball}" '$2==f || $2=="*"f {print tolower($1); exit}' "${LOCAL_TMP}/SHA256SUMS")"
  [ -n "${expected_tar_sha}" ] || die "SHA256SUMS missing entry for ${tarball}"
  actual_tar_sha="$(sha256_file "${LOCAL_TMP}/${tarball}")"
  [ "${actual_tar_sha}" = "${expected_tar_sha}" ] || die "Tarball SHA256 mismatch: expected ${expected_tar_sha}, got ${actual_tar_sha}"

  mkdir -p "${LOCAL_TMP}/extract"
  tar -xzf "${LOCAL_TMP}/${tarball}" -C "${LOCAL_TMP}/extract"

  local bin_path="${LOCAL_TMP}/extract/gatehouse"
  local sums_path="${LOCAL_TMP}/extract/SHA256SUMS"
  local meta_path="${LOCAL_TMP}/extract/RELEASE_META"
  [ -f "${bin_path}" ] || die "Extracted tarball missing 'gatehouse'"
  [ -f "${sums_path}" ] || die "Extracted tarball missing 'SHA256SUMS'"

  local expected_bin_sha actual_bin_sha
  expected_bin_sha="$(awk '$2=="gatehouse" || $2=="*gatehouse" {print tolower($1); exit}' "${sums_path}")"
  [ -n "${expected_bin_sha}" ] || die "Extracted SHA256SUMS missing entry for gatehouse"
  actual_bin_sha="$(sha256_file "${bin_path}")"
  [ "${actual_bin_sha}" = "${expected_bin_sha}" ] || die "Binary SHA256 mismatch: expected ${expected_bin_sha}, got ${actual_bin_sha}"
  chmod 0755 "${bin_path}"

  upload_and_install "${ver}" "${bin_path}" "${actual_bin_sha}" "${meta_path}"
}

cmd_local() {
  command -v go >/dev/null 2>&1 || die "Go toolchain is required for 'local' subcommand"
  local ver="${1:-}"
  local commit="unknown"
  local short_sha="local"
  if git -C "${REPO_ROOT}" rev-parse HEAD >/dev/null 2>&1; then
    commit="$(git -C "${REPO_ROOT}" rev-parse HEAD)"
    short_sha="$(git -C "${REPO_ROOT}" rev-parse --short=7 HEAD)"
  fi
  if [ -z "${ver}" ]; then
    ver="local-${short_sha}"
  fi
  validate_version "${ver}"

  LOCAL_TMP="$(mktemp -d)"
  local bin_path="${LOCAL_TMP}/gatehouse"
  local meta_path="${LOCAL_TMP}/RELEASE_META"

  log "Cross-compiling linux/amd64 binary (${ver})..."
  (
    cd "${REPO_ROOT}"
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
      -trimpath \
      -ldflags "-s -w -X main.version=${ver}" \
      -o "${bin_path}" \
      ./cmd/gatehouse
  )
  chmod 0755 "${bin_path}"
  local bin_sha
  bin_sha="$(sha256_file "${bin_path}")"
  local built_at
  built_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

  python3 -c 'import json,sys; print(json.dumps({"version":sys.argv[1],"commit":sys.argv[2],"short_sha":sys.argv[3],"built_at":sys.argv[4],"arch":"linux/amd64","binary_sha256":sys.argv[5]}, indent=2))' \
    "${ver}" "${commit}" "${short_sha}" "${built_at}" "${bin_sha}" > "${meta_path}"

  upload_and_install "${ver}" "${bin_path}" "${bin_sha}" "${meta_path}"
}

cmd_rollback() {
  local target="${1:-}"
  if [ -n "${target}" ]; then
    validate_version "${target}"
    ssh "${SSH_OPTS[@]}" "${SSH_HOST}" "'${REMOTE_HOME}/scripts/apply-release.sh' rollback '${target}'"
  else
    ssh "${SSH_OPTS[@]}" "${SSH_HOST}" "'${REMOTE_HOME}/scripts/apply-release.sh' rollback"
  fi
}

cmd_status() {
  ssh "${SSH_OPTS[@]}" "${SSH_HOST}" "'${REMOTE_HOME}/scripts/apply-release.sh' status"
}

cmd_list() {
  ssh "${SSH_OPTS[@]}" "${SSH_HOST}" "'${REMOTE_HOME}/scripts/apply-release.sh' list"
}

cmd_sync_scripts() {
  log "Syncing deploy scripts to ${SSH_HOST}:${REMOTE_HOME}..."
  ssh "${SSH_OPTS[@]}" "${SSH_HOST}" "mkdir -p '${REMOTE_HOME}/scripts'"
  scp -q "${SSH_OPTS[@]}" \
    "${SCRIPT_DIR}/apply-release.sh" \
    "${SCRIPT_DIR}/pull-from-registry.py" \
    "${SCRIPT_DIR}/pull-and-deploy.sh" \
    "${SSH_HOST}:${REMOTE_HOME}/scripts/"
  scp -q "${SSH_OPTS[@]}" \
    "${DEPLOY_ROOT}/mirror.env.example" \
    "${SSH_HOST}:${REMOTE_HOME}/mirror.env.example"
  ssh "${SSH_OPTS[@]}" "${SSH_HOST}" \
    "chmod 0755 '${REMOTE_HOME}/scripts/apply-release.sh' '${REMOTE_HOME}/scripts/pull-from-registry.py' '${REMOTE_HOME}/scripts/pull-and-deploy.sh' && chmod 0600 '${REMOTE_HOME}/mirror.env.example'"
  log "Scripts synced."
}

usage() {
  cat <<EOF
Usage: $(basename "$0") <command> [args]

Commands:
  pull [tag]             Trigger gpu1 to pull from domestic mirror & deploy (default tag: latest)
  check-mirror [tag]     Check remote mirror digest vs gpu1 current digest
  dry-run-mirror [tag]   Pull & validate config on gpu1 without restarting service
  release [ver|--latest] Download verified GitHub Release locally and push to gpu1
  local [ver]            Cross-compile local tree for linux/amd64 and push to gpu1
  rollback [ver]         Roll back gpu1 to previous version (or specified version)
  status                 Show gpu1 active binary, version, systemd and /healthz status
  list                   List installed release versions on gpu1
  sync-scripts           Upload latest deploy scripts & mirror.env.example to gpu1
EOF
}

CMD="${1:-}"
[ -n "${CMD}" ] || { usage >&2; exit 2; }
shift

case "${CMD}" in
  pull) cmd_pull "$@" ;;
  check-mirror) cmd_check_mirror "$@" ;;
  dry-run-mirror) cmd_dry_run_mirror "$@" ;;
  release) cmd_release "$@" ;;
  local) cmd_local "$@" ;;
  rollback) cmd_rollback "$@" ;;
  status) cmd_status "$@" ;;
  list) cmd_list "$@" ;;
  sync-scripts) cmd_sync_scripts "$@" ;;
  -h|--help|help) usage ;;
  *) usage >&2; die "Unknown command: ${CMD}" ;;
esac
