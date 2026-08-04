#!/bin/bash
# Offline install/update for the Photos AI (NimoOS in-house mlserver) system app.
# Extract the distribution bundle built by script/package-photos-ml.sh and run
# this script. Idempotent: re-running it updates to the bundled image version.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
APP_ID="nimoos-photos-ml"
APP_DIR="${APP_DIR:-/var/lib/nimoos/apps/${APP_ID}}"
# DataPath follows /etc/nimoos/photos.conf (photos' derived data can be moved
# to another disk); fall back to the legacy default if it can't be read.
# Note: once photos.conf is rewritten by Settings.Save() (viper), the key
# becomes lowercase "datapath", so the match must be case-insensitive.
CONF_DATA_PATH="$(awk -F' *= *' '{k=$1; gsub(/^[ \t]+|[ \t]+$/,"",k); if (tolower(k)=="datapath") print $2}' /etc/nimoos/photos.conf 2>/dev/null | tail -n1 || true)"
PHOTOS_DATA="${CONF_DATA_PATH:-/DATA/.system_data/photos}"
ML_CACHE="${PHOTOS_DATA}/ml-cache"
OV_CACHE="${ML_CACHE}/ov-cache"
IMAGE_TAR="${HERE}/immich-ml.tar"
MODELS_TAR="${HERE}/ml-models.tar.gz"
IMAGE_REF="localhost/nimoos-photos-ml:bundled"

# ── flavor and iGPU vendor auto-detection ───────────────────────────────
# One image now covers every device (CPU + Intel iGPU via OpenVINO EP inside
# the container, MLSERVER_DEVICE=auto picks whichever is available at
# runtime). detect_gpu_vendor's job is no longer "reject a mismatched
# flavor/hardware pairing" -- there is only one flavor now -- it just decides
# whether to lay down the Intel /dev/dri device-passthrough override.
FLAVOR="$(cat "${HERE}/FLAVOR" 2>/dev/null || echo universal)"

detect_gpu_vendor() {
  local f v
  for f in /sys/class/drm/card*/device/vendor; do
    [[ -r "$f" ]] || continue
    v="$(cat "$f")"
    case "$v" in
      0x8086) echo intel; return ;;
      0x1002) echo amd;   return ;;
    esac
  done
  echo none
}

VENDOR="$(detect_gpu_vendor)"
echo "==> bundle flavor=${FLAVOR}, detected iGPU vendor=${VENDOR}"

# Legacy per-flavor bundles (cpu/openvino/rocm) shipped the official
# immich-machine-learning image and needed MACHINE_LEARNING_* env vars; this
# installer's compose file now hardcodes MLSERVER_* for the single universal
# image, so an old-style bundle here would boot the wrong image with the
# wrong environment. Fail loudly instead of silently misconfiguring it.
if [[ "${FLAVOR}" != universal ]]; then
  echo "✗ this installer only supports a 'universal' bundle (found FLAVOR=${FLAVOR})." >&2
  echo "  Re-download/rebuild the universal bundle (script/package-photos-ml.sh) -- the" >&2
  echo "  old per-flavor (cpu/openvino/rocm) bundles are not compatible with this" >&2
  echo "  installer/compose version." >&2
  exit 1
fi

PING_URL="http://127.0.0.1:3003/ping"
# 300s: only the very first run against a fresh ml-cache pays the OpenVINO EP
# compile cost (persisted afterwards under ml-cache/ov-cache); every
# subsequent start is fast and returns long before the deadline.
PING_TIMEOUT=300   # seconds
FORCE_MODELS="${FORCE_MODELS:-}"   # =1 forces overwriting an existing model cache

[[ -f "${IMAGE_TAR}" ]] || { echo "✗ image bundle not found: ${IMAGE_TAR}" >&2; exit 1; }

echo "==> [1/5] Loading offline image ${IMAGE_REF} ..."
docker load -i "${IMAGE_TAR}"

echo "==> [2/5] Deploying compose to ${APP_DIR} ..."
mkdir -p "${APP_DIR}" "${ML_CACHE}"

# ov-cache holds the OpenVINO GPU EP's compiled-kernel blobs, persisted
# across container restarts/upgrades so the (expensive, one-time) EP
# compilation isn't repeated on every start. The container runs as root, so
# it can write here regardless of ownership, but make sure the directory
# exists up front (and is owned by the invoking user, not left root:root from
# compose auto-creating bind paths) so a host-side re-run of the golden/bench
# tooling as a normal user -- or a future non-root image -- doesn't trip over
# a missing/unwritable dir. Upgrade path: if ov-cache already exists (e.g.
# from an earlier host-side mlserver run), leave its ownership alone --
# don't churn a cache that's already correct.
mkdir -p "${OV_CACHE}"
if [[ -n "${SUDO_USER:-}" ]] && [[ "$(stat -c '%U' "${OV_CACHE}")" == root ]]; then
  chown "${SUDO_USER}:$(id -gn "${SUDO_USER}")" "${OV_CACHE}"
fi

cp "${HERE}/docker-compose.yml" "${APP_DIR}/docker-compose.yml"

# Bake the actual ml-cache path into .env; compose (project dir = APP_DIR)
# reads it automatically, and both AppManagement and manual restarts pick up
# the same path.
printf 'NIMOOS_PHOTOS_ML_CACHE=%s\n' "${ML_CACHE}" > "${APP_DIR}/.env"

COMPOSE_FILES=(-f "${APP_DIR}/docker-compose.yml")
if [[ "${VENDOR}" == intel ]]; then
  echo "    Intel iGPU detected: installing OpenVINO device-passthrough override."
  cp "${HERE}/overrides/openvino.yml" "${APP_DIR}/docker-compose.override.yml"
  COMPOSE_FILES+=(-f "${APP_DIR}/docker-compose.override.yml")
else
  echo "    No Intel iGPU detected (vendor=${VENDOR}): CPU mode, no device override."
  rm -f "${APP_DIR}/docker-compose.override.yml"
fi

# Model cache: if the bundle carries ml-models.tar.gz, lay it into ml-cache
# for a zero-network first run. Skip if models already exist (idempotent,
# reruns are fast); FORCE_MODELS=1 forces an overwrite.
echo "==> [3/5] Preparing model cache ..."
if [[ -f "${MODELS_TAR}" ]]; then
  if [[ -z "${FORCE_MODELS}" ]] && [[ -d "${ML_CACHE}/clip" ]]; then
    echo "    Models already present, skipping (FORCE_MODELS=1 to force overwrite)."
  else
    echo "    Extracting models to ${ML_CACHE} ..."
    tar -xzf "${MODELS_TAR}" -C "${PHOTOS_DATA}"
    echo "    Models in place ($(du -sh "${ML_CACHE}" 2>/dev/null | cut -f1))."
  fi
else
  echo "    This bundle carries no models (lightweight bundle); first run will download over the network."
fi

# Project name is fixed to APP_ID so the container name matches other
# machines (nimoos-photos-ml-server-1).
echo "==> [4/5] Starting ${APP_ID} ..."
docker compose -p "${APP_ID}" "${COMPOSE_FILES[@]}" up -d

echo "==> [5/5] Waiting for ML to be ready (up to ${PING_TIMEOUT}s, polling ${PING_URL}) ..."
deadline=$(( SECONDS + PING_TIMEOUT ))
while (( SECONDS < deadline )); do
  if curl -fsS "${PING_URL}" 2>/dev/null | grep -q pong; then
    echo "✓ Photos AI is ready and running."
    exit 0
  fi
  sleep 2
done

echo "✗ Timed out: ML did not become ready within ${PING_TIMEOUT}s." >&2
echo "  Troubleshoot: docker logs ${APP_ID}-server-1" >&2
exit 1
