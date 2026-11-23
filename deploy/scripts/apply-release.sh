#!/usr/bin/env bash
set -euo pipefail

GATEHOUSE_HOME="${GATEHOUSE_HOME:-/AIOS/sglang-kunlun-v0522-1017/deploy-dsv41-1017/gateway/gatehouse}"
GATEHOUSE_SERVICE="${GATEHOUSE_SERVICE:-dsv41-gatehouse.service}"
GATEHOUSE_CONFIG="${GATEHOUSE_CONFIG:-${GATEHOUSE_HOME}/config.yaml}"
HEALTHZ_URL="${HEALTHZ_URL:-http://127.0.0.1:18011/healthz}"
ADMIN_URL="${ADMIN_URL:-http://127.0.0.1:18002/admin/}"
HEALTH_TIMEOUT="${HEALTH_TIMEOUT:-25}"
KEEP_RELEASES="${KEEP_RELEASES:-5}"
SYSTEMCTL_CMD="${SYSTEMCTL_CMD:-systemctl}"
JOURNALCTL_CMD="${JOURNALCTL_CMD:-journalctl}"
SKIP_SERVICE_CHECK="${SKIP_SERVICE_CHECK:-0}"

BIN_DIR="${GATEHOUSE_HOME}/bin"
BIN_LINK="${BIN_DIR}/gatehouse"
RELEASES_DIR="${GATEHOUSE_HOME}/releases"
CUR_VER_FILE="${GATEHOUSE_HOME}/.current_version"
PREV_VER_FILE="${GATEHOUSE_HOME}/.previous_version"
DIGEST_FILE="${GATEHOUSE_HOME}/.current_digest"
LOCK_FILE="${GATEHOUSE_HOME}/.deploy.lock"
LOCK_DIR="${GATEHOUSE_HOME}/.deploy.lock.d"

STAGING_DIR=""
TMP_REL_DIR=""
HAVE_DIR_LOCK=0

cleanup() {
  if [ -n "${STAGING_DIR}" ] && [ -d "${STAGING_DIR}" ]; then
    rm -rf "${STAGING_DIR}"
  fi
  if [ -n "${TMP_REL_DIR}" ] && [ -d "${TMP_REL_DIR}" ]; then
    rm -rf "${TMP_REL_DIR}"
  fi
  if [ "${HAVE_DIR_LOCK}" -eq 1 ] && [ -d "${LOCK_DIR}" ]; then
    rmdir "${LOCK_DIR}" 2>/dev/null || true
  fi
}
trap cleanup EXIT

log() {
  printf '[apply-release] %s\n' "$*"
}

die() {
  printf '[apply-release] ERROR: %s\n' "$*" >&2
  exit 1
}

validate_version() {
  local ver="$1"
  if [[ ! "${ver}" =~ ^[A-Za-z0-9._-]+$ ]] || [ "${ver}" = "." ] || [ "${ver}" = ".." ]; then
    die "Invalid version '${ver}'. Allowed characters: A-Z a-z 0-9 . _ -"
  fi
}

sha256_file() {
  local path="$1"
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$path" | awk '{print tolower($1)}'
  else
    shasum -a 256 "$path" | awk '{print tolower($1)}'
  fi
}

acquire_lock() {
  mkdir -p "${GATEHOUSE_HOME}"
  if command -v flock >/dev/null 2>&1; then
    exec 9>"${LOCK_FILE}"
    if ! flock -n 9; then
      die "Another deployment is already in progress (lock: ${LOCK_FILE})"
    fi
  else
    if ! mkdir "${LOCK_DIR}" 2>/dev/null; then
      die "Another deployment is already in progress (lock: ${LOCK_DIR})"
    fi
    HAVE_DIR_LOCK=1
  fi
}

atomic_swap_symlink() {
  local rel_target="$1"
  local link_path="$2"
  local tmp_link="${BIN_DIR}/.gatehouse.next.$$"
  rm -f "${tmp_link}"
  ln -s "${rel_target}" "${tmp_link}"
  if [ "$(uname -s)" = "Linux" ]; then
    mv -Tf "${tmp_link}" "${link_path}"
  else
    mv -f "${tmp_link}" "${link_path}"
  fi
}

current_version() {
  if [ -L "${BIN_LINK}" ]; then
    local target
    target="$(readlink "${BIN_LINK}")"
    basename "$(dirname "${target}")"
    return 0
  fi
  if [ -f "${CUR_VER_FILE}" ]; then
    tr -d '[:space:]' < "${CUR_VER_FILE}"
    return 0
  fi
  printf ''
}

archive_baseline_if_regular_file() {
  if [ -e "${BIN_LINK}" ] && [ ! -L "${BIN_LINK}" ]; then
    local base_ver="baseline-$(date +%Y%m%d%H%M%S)"
    local base_dir="${RELEASES_DIR}/${base_ver}"
    mkdir -p "${base_dir}"
    cp -a "${BIN_LINK}" "${base_dir}/gatehouse"
    chmod 0755 "${base_dir}/gatehouse"
    local base_sha
    base_sha="$(sha256_file "${base_dir}/gatehouse")"
    printf '%s  gatehouse\n' "${base_sha}" > "${base_dir}/SHA256SUMS"
    python3 -c 'import json,sys; print(json.dumps({"version":sys.argv[1],"binary_sha256":sys.argv[2],"note":"archived pre-CD baseline"}, indent=2))' \
      "${base_ver}" "${base_sha}" > "${base_dir}/RELEASE_META"
    atomic_swap_symlink "../releases/${base_ver}/gatehouse" "${BIN_LINK}"
    printf '%s\n' "${base_ver}" > "${CUR_VER_FILE}"
    log "Archived existing regular binary to releases/${base_ver}/gatehouse"
  fi
}

check_health() {
  local elapsed=0
  local admin_code=""
  local health_json=""
  while [ "${elapsed}" -lt "${HEALTH_TIMEOUT}" ]; do
    local svc_ok=1
    if [ "${SKIP_SERVICE_CHECK}" != "1" ]; then
      if ! "${SYSTEMCTL_CMD}" is-active --quiet "${GATEHOUSE_SERVICE}"; then
        svc_ok=0
      fi
    fi

    if [ "${svc_ok}" -eq 1 ]; then
      if health_json="$(curl -sf --max-time 3 "${HEALTHZ_URL}" 2>/dev/null)"; then
        if printf '%s' "${health_json}" | python3 -c 'import json,sys; d=json.load(sys.stdin); sys.exit(0 if d.get("status")=="ok" else 1)' 2>/dev/null; then
          admin_code="$(curl -s --max-time 3 -o /dev/null -w '%{http_code}' "${ADMIN_URL}" 2>/dev/null || true)"
          if [[ "${admin_code}" =~ ^(200|301|302)$ ]]; then
            log "Health check passed (${HEALTHZ_URL} ok, ${ADMIN_URL} HTTP ${admin_code})"
            return 0
          fi
        fi
      fi
    fi
    sleep 1
    elapsed=$((elapsed + 1))
  done
  log "Health check failed after ${HEALTH_TIMEOUT}s (last admin HTTP: ${admin_code:-none})"
  return 1
}

prune_releases() {
  local keep_cur="$1"
  local keep_prev="$2"
  python3 - "$RELEASES_DIR" "$KEEP_RELEASES" "$keep_cur" "$keep_prev" <<'PY'
import os, shutil, sys

releases_dir = sys.argv[1]
keep_count = int(sys.argv[2])
keep_set = set(x for x in sys.argv[3:] if x)

entries = []
for name in os.listdir(releases_dir):
    if name.startswith("."):
        continue
    full = os.path.join(releases_dir, name)
    if not os.path.isdir(full):
        continue
    if name.startswith("baseline-") or name in keep_set:
        continue
    entries.append((os.path.getmtime(full), name, full))

entries.sort(key=lambda item: item[0], reverse=True)
for _, name, full in entries[keep_count:]:
    print("[apply-release] Pruning old release:", name)
    shutil.rmtree(full)
PY
}

cmd_install() {
  if [ "$#" -lt 2 ]; then
    die "Usage: $0 install <version> <binary-path> [--sha256 <hex>] [--meta <json-path>] [--no-restart]"
  fi
  local version="$1"
  local bin_src="$2"
  shift 2

  local expected_sha=""
  local meta_src=""
  local no_restart=0

  while [ "$#" -gt 0 ]; do
    case "$1" in
      --sha256)
        [ "$#" -ge 2 ] || die "--sha256 requires a value"
        expected_sha="$(printf '%s' "$2" | tr '[:upper:]' '[:lower:]')"
        shift 2
        ;;
      --meta)
        [ "$#" -ge 2 ] || die "--meta requires a value"
        meta_src="$2"
        shift 2
        ;;
      --no-restart)
        no_restart=1
        shift
        ;;
      *)
        die "Unknown option for install: $1"
        ;;
    esac
  done

  validate_version "${version}"
  [ -f "${bin_src}" ] || die "Binary not found: ${bin_src}"
  [ -f "${GATEHOUSE_CONFIG}" ] || die "Config file not found: ${GATEHOUSE_CONFIG}"

  local actual_sha
  actual_sha="$(sha256_file "${bin_src}")"
  if [ -n "${expected_sha}" ] && [ "${actual_sha}" != "${expected_sha}" ]; then
    die "SHA256 mismatch for ${bin_src}: expected ${expected_sha}, got ${actual_sha}"
  fi

  acquire_lock
  mkdir -p "${BIN_DIR}" "${RELEASES_DIR}"

  STAGING_DIR="${RELEASES_DIR}/.staging-$$"
  mkdir -p "${STAGING_DIR}"
  local staged_bin="${STAGING_DIR}/gatehouse"
  cp "${bin_src}" "${staged_bin}"
  chmod 0755 "${staged_bin}"

  log "Validating staged binary (${version})..."
  "${staged_bin}" version
  "${staged_bin}" config check --config "${GATEHOUSE_CONFIG}"

  archive_baseline_if_regular_file
  local prev_ver
  prev_ver="$(current_version)"

  TMP_REL_DIR="${RELEASES_DIR}/.tmp-${version}-$$"
  mkdir -p "${TMP_REL_DIR}"
  mv "${staged_bin}" "${TMP_REL_DIR}/gatehouse"
  chmod 0755 "${TMP_REL_DIR}/gatehouse"
  printf '%s  gatehouse\n' "${actual_sha}" > "${TMP_REL_DIR}/SHA256SUMS"
  if [ -n "${meta_src}" ] && [ -f "${meta_src}" ]; then
    cp "${meta_src}" "${TMP_REL_DIR}/RELEASE_META"
    chmod 0644 "${TMP_REL_DIR}/RELEASE_META"
  else
    python3 -c 'import json,sys,time; print(json.dumps({"version":sys.argv[1],"binary_sha256":sys.argv[2],"installed_at":time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())}, indent=2))' \
      "${version}" "${actual_sha}" > "${TMP_REL_DIR}/RELEASE_META"
  fi

  local target_dir="${RELEASES_DIR}/${version}"
  rm -rf "${target_dir}"
  mv "${TMP_REL_DIR}" "${target_dir}"
  TMP_REL_DIR=""

  atomic_swap_symlink "../releases/${version}/gatehouse" "${BIN_LINK}"
  log "Switched ${BIN_LINK} -> ../releases/${version}/gatehouse"

  if [ "${no_restart}" -eq 1 ]; then
    if [ -n "${prev_ver}" ] && [ "${prev_ver}" != "${version}" ]; then
      printf '%s\n' "${prev_ver}" > "${PREV_VER_FILE}"
    fi
    printf '%s\n' "${version}" > "${CUR_VER_FILE}"
    log "Installed ${version} (--no-restart set; service not restarted)."
    return 0
  fi

  log "Restarting ${GATEHOUSE_SERVICE}..."
  "${SYSTEMCTL_CMD}" restart "${GATEHOUSE_SERVICE}"

  if ! check_health; then
    log "Service unhealthy after deploying ${version}; recent logs:"
    if [ "${SKIP_SERVICE_CHECK}" != "1" ]; then
      "${JOURNALCTL_CMD}" -u "${GATEHOUSE_SERVICE}" -n 40 --no-pager >&2 || true
    fi
    if [ -n "${prev_ver}" ] && [ "${prev_ver}" != "${version}" ] && [ -x "${RELEASES_DIR}/${prev_ver}/gatehouse" ]; then
      log "Auto-rolling back to previous version ${prev_ver}..."
      atomic_swap_symlink "../releases/${prev_ver}/gatehouse" "${BIN_LINK}"
      "${SYSTEMCTL_CMD}" restart "${GATEHOUSE_SERVICE}" || true
      if check_health; then
        printf '%s\n' "${prev_ver}" > "${CUR_VER_FILE}"
        die "Deployment of ${version} failed health check and was automatically rolled back to ${prev_ver}."
      else
        die "Deployment of ${version} failed AND rollback to ${prev_ver} also failed health check!"
      fi
    fi
    die "Deployment of ${version} failed health check (no valid previous version to roll back to)."
  fi

  if [ -n "${prev_ver}" ] && [ "${prev_ver}" != "${version}" ]; then
    printf '%s\n' "${prev_ver}" > "${PREV_VER_FILE}"
  fi
  printf '%s\n' "${version}" > "${CUR_VER_FILE}"

  local saved_prev=""
  [ -f "${PREV_VER_FILE}" ] && saved_prev="$(tr -d '[:space:]' < "${PREV_VER_FILE}")"
  prune_releases "${version}" "${saved_prev}"
  log "Successfully deployed ${version} (sha256: ${actual_sha})"
}

cmd_rollback() {
  local target="${1:-}"
  acquire_lock

  local cur_ver
  cur_ver="$(current_version)"

  if [ -z "${target}" ]; then
    if [ -f "${PREV_VER_FILE}" ]; then
      target="$(tr -d '[:space:]' < "${PREV_VER_FILE}")"
    fi
    if [ -z "${target}" ] || [ "${target}" = "${cur_ver}" ] || [ ! -x "${RELEASES_DIR}/${target}/gatehouse" ]; then
      target="$(python3 - "${RELEASES_DIR}" "${cur_ver}" <<'PY'
import os, sys
rdir, cur = sys.argv[1], sys.argv[2]
cands = []
if os.path.isdir(rdir):
    for name in os.listdir(rdir):
        if name.startswith(".") or name == cur:
            continue
        b = os.path.join(rdir, name, "gatehouse")
        if os.path.isfile(b) and os.access(b, os.X_OK):
            cands.append((os.path.getmtime(os.path.join(rdir, name)), name))
cands.sort(reverse=True)
print(cands[0][1] if cands else "")
PY
)"
    fi
  fi

  [ -n "${target}" ] || die "No rollback target version available in ${RELEASES_DIR}"
  validate_version "${target}"

  local target_bin="${RELEASES_DIR}/${target}/gatehouse"
  [ -x "${target_bin}" ] || die "Target rollback binary not executable: ${target_bin}"

  log "Validating rollback target ${target}..."
  "${target_bin}" version
  "${target_bin}" config check --config "${GATEHOUSE_CONFIG}"

  atomic_swap_symlink "../releases/${target}/gatehouse" "${BIN_LINK}"
  log "Restarting ${GATEHOUSE_SERVICE} on ${target}..."
  "${SYSTEMCTL_CMD}" restart "${GATEHOUSE_SERVICE}"

  if ! check_health; then
    if [ "${SKIP_SERVICE_CHECK}" != "1" ]; then
      "${JOURNALCTL_CMD}" -u "${GATEHOUSE_SERVICE}" -n 40 --no-pager >&2 || true
    fi
    die "Rollback to ${target} failed health check!"
  fi

  if [ -n "${cur_ver}" ] && [ "${cur_ver}" != "${target}" ]; then
    printf '%s\n' "${cur_ver}" > "${PREV_VER_FILE}"
  fi
  printf '%s\n' "${target}" > "${CUR_VER_FILE}"
  log "Rolled back successfully to ${target}"
}

cmd_list() {
  local cur_ver
  cur_ver="$(current_version)"
  local prev_ver=""
  [ -f "${PREV_VER_FILE}" ] && prev_ver="$(tr -d '[:space:]' < "${PREV_VER_FILE}")"

  python3 - "${RELEASES_DIR}" "${cur_ver}" "${prev_ver}" <<'PY'
import json, os, sys, time
rdir, cur, prev = sys.argv[1], sys.argv[2], sys.argv[3]
if not os.path.isdir(rdir):
    print("No releases directory found:", rdir)
    sys.exit(0)
rows = []
for name in os.listdir(rdir):
    if name.startswith("."):
        continue
    full = os.path.join(rdir, name)
    if not os.path.isdir(full):
        continue
    rows.append((os.path.getmtime(full), name, full))
rows.sort(reverse=True)
if not rows:
    print("No releases installed.")
    sys.exit(0)
print("%-3s %-28s %-14s %-22s %s" % ("", "VERSION", "SHA256", "MODIFIED", "NOTE"))
for mtime, name, full in rows:
    mark = "*" if name == cur else ("<-" if name == prev else "")
    sha = "-"
    note = ""
    meta_path = os.path.join(full, "RELEASE_META")
    if os.path.isfile(meta_path):
        try:
            with open(meta_path, "r", encoding="utf-8") as fp:
                m = json.load(fp)
            sha = (m.get("binary_sha256") or "-")[:12]
            note = m.get("short_sha") or m.get("note") or ""
        except Exception:
            pass
    ts = time.strftime("%Y-%m-%d %H:%M:%S", time.localtime(mtime))
    print("%-3s %-28s %-14s %-22s %s" % (mark, name, sha, ts, note))
PY
}

cmd_status() {
  local cur_ver
  cur_ver="$(current_version)"
  local prev_ver=""
  [ -f "${PREV_VER_FILE}" ] && prev_ver="$(tr -d '[:space:]' < "${PREV_VER_FILE}")"
  local cur_digest=""
  [ -f "${DIGEST_FILE}" ] && cur_digest="$(tr -d '[:space:]' < "${DIGEST_FILE}")"

  printf 'GATEHOUSE_HOME: %s\n' "${GATEHOUSE_HOME}"
  if [ -L "${BIN_LINK}" ]; then
    printf 'Binary symlink: %s -> %s\n' "${BIN_LINK}" "$(readlink "${BIN_LINK}")"
  elif [ -e "${BIN_LINK}" ]; then
    printf 'Binary file:    %s (regular file, not symlink)\n' "${BIN_LINK}"
  else
    printf 'Binary file:    %s (missing)\n' "${BIN_LINK}"
  fi

  if [ -x "${BIN_LINK}" ]; then
    printf 'CLI version:    %s\n' "$("${BIN_LINK}" version 2>&1 || true)"
  fi
  printf 'Active release: %s\n' "${cur_ver:-<unknown>}"
  printf 'Previous rel:   %s\n' "${prev_ver:-<none>}"
  printf 'Mirror digest:  %s\n' "${cur_digest:-<none>}"

  if [ "${SKIP_SERVICE_CHECK}" != "1" ]; then
    local active_state main_pid
    active_state="$("${SYSTEMCTL_CMD}" is-active "${GATEHOUSE_SERVICE}" 2>/dev/null || true)"
    main_pid="$("${SYSTEMCTL_CMD}" show -p MainPID --value "${GATEHOUSE_SERVICE}" 2>/dev/null || true)"
    printf 'Systemd unit:   %s (state=%s, MainPID=%s)\n' "${GATEHOUSE_SERVICE}" "${active_state:-unknown}" "${main_pid:-0}"
  fi

  local health_json admin_code
  health_json="$(curl -sf --max-time 3 "${HEALTHZ_URL}" 2>/dev/null || echo 'UNREACHABLE')"
  admin_code="$(curl -s --max-time 3 -o /dev/null -w '%{http_code}' "${ADMIN_URL}" 2>/dev/null || echo '000')"
  printf 'Healthz (%s): %s\n' "${HEALTHZ_URL}" "${health_json}"
  printf 'Admin (%s):   HTTP %s\n' "${ADMIN_URL}" "${admin_code}"
}

usage() {
  cat <<EOF
Usage: $(basename "$0") <command> [args]

Commands:
  install <version> <binary-path> [--sha256 <hex>] [--meta <json-path>] [--no-restart]
  rollback [target-version]
  list
  status
EOF
}

CMD="${1:-}"
[ -n "${CMD}" ] || { usage >&2; exit 2; }
shift

case "${CMD}" in
  install) cmd_install "$@" ;;
  rollback) cmd_rollback "$@" ;;
  list) cmd_list "$@" ;;
  status) cmd_status "$@" ;;
  -h|--help|help) usage ;;
  *) usage >&2; die "Unknown command: ${CMD}" ;;
esac
