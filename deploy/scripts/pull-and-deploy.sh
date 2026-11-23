#!/usr/bin/env bash
set -euo pipefail

GATEHOUSE_HOME="${GATEHOUSE_HOME:-/AIOS/sglang-kunlun-v0522-1017/deploy-dsv41-1017/gateway/gatehouse}"
GATEHOUSE_CONFIG="${GATEHOUSE_CONFIG:-${GATEHOUSE_HOME}/config.yaml}"
MIRROR_ENV_FILE="${MIRROR_ENV_FILE:-${GATEHOUSE_HOME}/mirror.env}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

MODE="deploy"
TAG=""

while [ "$#" -gt 0 ]; do
  case "$1" in
    --check)
      MODE="check"
      shift
      ;;
    --dry-run)
      MODE="dry-run"
      shift
      ;;
    --if-changed)
      MODE="if-changed"
      shift
      ;;
    -h|--help)
      cat <<EOF
Usage: $(basename "$0") [--check | --dry-run | --if-changed] [tag]

Reads mirror credentials from ${MIRROR_ENV_FILE} and pulls the Gatehouse
artifact image from the configured domestic OCI registry (ACR/CCR).
EOF
      exit 0
      ;;
    -*)
      echo "[pull-and-deploy] Unknown option: $1" >&2
      exit 2
      ;;
    *)
      TAG="$1"
      shift
      ;;
  esac
done

if [ ! -f "${MIRROR_ENV_FILE}" ]; then
  echo "[pull-and-deploy] Missing ${MIRROR_ENV_FILE}." >&2
  echo "Copy ${GATEHOUSE_HOME}/mirror.env.example to ${MIRROR_ENV_FILE}, fill CN_REGISTRY_* values, and chmod 600." >&2
  exit 2
fi

set -a
# shellcheck source=/dev/null
source "${MIRROR_ENV_FILE}"
set +a

TAG="${TAG:-${MIRROR_TAG:-latest}}"
CN_REGISTRY_IMAGE="${CN_REGISTRY_IMAGE:-}"
CN_REGISTRY_USERNAME="${CN_REGISTRY_USERNAME:-}"
CN_REGISTRY_PASSWORD="${CN_REGISTRY_PASSWORD:-}"
MIRROR_DNS="${MIRROR_DNS:-223.5.5.5,119.29.29.29}"

if [ -z "${CN_REGISTRY_IMAGE}" ] || [ -z "${CN_REGISTRY_USERNAME}" ] || [ -z "${CN_REGISTRY_PASSWORD}" ] || [[ "${CN_REGISTRY_IMAGE}" == *"<"* ]] || [[ "${CN_REGISTRY_USERNAME}" == *"<"* ]] || [[ "${CN_REGISTRY_PASSWORD}" == *"<"* ]]; then
  echo "[pull-and-deploy] Domestic mirror credentials in ${MIRROR_ENV_FILE} are incomplete or still contain placeholders." >&2
  exit 2
fi

export CN_REGISTRY_USERNAME CN_REGISTRY_PASSWORD MIRROR_DNS

PULL_TMP=""
cleanup() {
  if [ -n "${PULL_TMP}" ] && [ -d "${PULL_TMP}" ]; then
    rm -rf "${PULL_TMP}"
  fi
}
trap cleanup EXIT

DIGEST_FILE="${GATEHOUSE_HOME}/.current_digest"
CURRENT_DIGEST=""
if [ -f "${DIGEST_FILE}" ]; then
  CURRENT_DIGEST="$(tr -d '[:space:]' < "${DIGEST_FILE}")"
fi

if [ "${MODE}" = "check" ] || [ "${MODE}" = "if-changed" ]; then
  CHECK_JSON="$(python3 "${SCRIPT_DIR}/pull-from-registry.py" --image "${CN_REGISTRY_IMAGE}" --tag "${TAG}" --dns "${MIRROR_DNS}" --check-only)"
  REMOTE_DIGEST="$(printf '%s' "${CHECK_JSON}" | python3 -c 'import json,sys; print(json.load(sys.stdin)["digest"])')"
  if [ "${MODE}" = "check" ]; then
    echo "Mirror image:   ${CN_REGISTRY_IMAGE}:${TAG}"
    echo "Remote digest:  ${REMOTE_DIGEST}"
    echo "Current digest: ${CURRENT_DIGEST:-<none>}"
    if [ "${REMOTE_DIGEST}" = "${CURRENT_DIGEST}" ]; then
      echo "Status:         up-to-date"
    else
      echo "Status:         update-available"
    fi
    exit 0
  fi
  if [ -n "${CURRENT_DIGEST}" ] && [ "${REMOTE_DIGEST}" = "${CURRENT_DIGEST}" ]; then
    echo "[pull-and-deploy] Already up to date on digest ${REMOTE_DIGEST}; skipping."
    exit 0
  fi
  echo "[pull-and-deploy] New digest detected (${CURRENT_DIGEST:-none} -> ${REMOTE_DIGEST}); pulling ${TAG}..."
fi

mkdir -p "${GATEHOUSE_HOME}/releases"
PULL_TMP="$(mktemp -d "${GATEHOUSE_HOME}/releases/.pull-XXXXXX")"

PULL_JSON="$(python3 "${SCRIPT_DIR}/pull-from-registry.py" --image "${CN_REGISTRY_IMAGE}" --tag "${TAG}" --dns "${MIRROR_DNS}" --out-dir "${PULL_TMP}")"

read_field() {
  printf '%s' "${PULL_JSON}" | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d.get(sys.argv[1],""))' "$1"
}

VERSION="$(read_field version)"
DIGEST="$(read_field digest)"
BIN_SHA="$(read_field binary_sha256)"
BIN_PATH="$(read_field binary_path)"
META_PATH="$(read_field meta_path)"

if [ -z "${VERSION}" ] || [ "${VERSION}" = "latest" ]; then
  SHORT_DIGEST="$(printf '%s' "${DIGEST#sha256:}" | cut -c1-12)"
  VERSION="${TAG}-${SHORT_DIGEST}"
fi

if [ "${MODE}" = "dry-run" ]; then
  echo "[pull-and-deploy] Dry-run verification passed:"
  echo "  version:       ${VERSION}"
  echo "  digest:        ${DIGEST}"
  echo "  binary_sha256: ${BIN_SHA}"
  "${BIN_PATH}" version
  "${BIN_PATH}" config check --config "${GATEHOUSE_CONFIG}"
  exit 0
fi

INSTALL_ARGS=("${VERSION}" "${BIN_PATH}" "--sha256" "${BIN_SHA}")
if [ -n "${META_PATH}" ] && [ -f "${META_PATH}" ]; then
  INSTALL_ARGS+=("--meta" "${META_PATH}")
fi

"${SCRIPT_DIR}/apply-release.sh" install "${INSTALL_ARGS[@]}"
printf '%s\n' "${DIGEST}" > "${DIGEST_FILE}"
echo "[pull-and-deploy] Recorded current mirror digest: ${DIGEST}"
