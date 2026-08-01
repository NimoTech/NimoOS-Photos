#!/bin/bash
# Build the Photos AI (immich-machine-learning) offline distribution bundle.
# Usage: script/package-photos-ml.sh <openvino|rocm|cpu> [output dir=./dist]
# Output: <output dir>/photos-ml-<flavor>-<IMMICH_VER>.tar.gz
# Requires: docker, curl, internet access (ghcr.io + HF (HF_ENDPOINT can point
# to a mirror) + modelscope.cn)
# Note: the rocm image is >=35GiB, reserve enough disk on the packaging
# machine; the three model caches together are about 5-7GiB.
set -euo pipefail

FLAVOR="${1:?Usage: package-photos-ml.sh <openvino|rocm|cpu> [output dir]}"
OUT="${2:-./dist}"
IMMICH_VER="v2.7.5"
REF="localhost/nimoos-photos-ml:bundled"
HERE="$(cd "$(dirname "$0")/.." && pwd)"   # repo root
DEPLOY="${HERE}/deploy/ml"
WARM_PORT="13003"

case "${FLAVOR}" in
  openvino) TAG="${IMMICH_VER}-openvino" ;;
  rocm)     TAG="${IMMICH_VER}-rocm" ;;
  cpu)      TAG="${IMMICH_VER}" ;;
  *) echo "✗ unknown flavor: ${FLAVOR}" >&2; exit 1 ;;
esac
SRC="ghcr.io/immich-app/immich-machine-learning:${TAG}"

# Pull the currently pinned model names from the Go constants, so the bundle
# and the code always stay in sync.
CLIP_MODEL="$(grep -oP 'CLIPModelName = "\K[^"]+' "${HERE}/common/constants.go")"
FACE_MODEL="$(grep -oP 'FaceModelName = "\K[^"]+' "${HERE}/common/constants.go")"
OCR_MODEL="$(grep -oP 'OCRModelName  = "\K[^"]+' "${HERE}/common/constants.go")"
[ -n "${CLIP_MODEL}" ] || { echo "✗ failed to read CLIPModelName from common/constants.go" >&2; exit 1; }
[ -n "${FACE_MODEL}" ] || { echo "✗ failed to read FaceModelName from common/constants.go" >&2; exit 1; }
[ -n "${OCR_MODEL}" ]  || { echo "✗ failed to read OCRModelName from common/constants.go" >&2; exit 1; }
echo "==> Models: clip=${CLIP_MODEL} face=${FACE_MODEL} ocr=${OCR_MODEL}"

STAGE="$(mktemp -d)"
# Warm cache directory: by default a temp dir that's removed when done; set
# PHOTOS_ML_WARM_DIR to a persistent directory so a failed run can resume
# from the HF cache instead of re-downloading several GB of models from scratch.
if [[ -n "${PHOTOS_ML_WARM_DIR:-}" ]]; then
  WARM="${PHOTOS_ML_WARM_DIR}"
  mkdir -p "${WARM}"
  KEEP_WARM=1
else
  WARM="$(mktemp -d)"
  KEEP_WARM=""
fi
trap 'docker rm -f photos-ml-warm >/dev/null 2>&1 || true; rm -rf "${STAGE}"; [[ -z "${KEEP_WARM}" ]] && rm -rf "${WARM}"' EXIT

echo "==> [1/4] Pulling and retagging ${SRC} ..."
docker pull "${SRC}"
docker tag "${SRC}" "${REF}"
docker save -o "${STAGE}/immich-ml.tar" "${REF}"

echo "==> [2/4] Warming the model cache (temp container downloads over the network, CPU mode is fine) ..."
mkdir -p "${WARM}/ml-cache"
docker rm -f photos-ml-warm >/dev/null 2>&1 || true
docker run -d --name photos-ml-warm \
  -p "127.0.0.1:${WARM_PORT}:3003" \
  -v "${WARM}/ml-cache":/cache \
  -e MACHINE_LEARNING_CACHE_FOLDER=/cache \
  ${HF_ENDPOINT:+-e HF_ENDPOINT="${HF_ENDPOINT}"} \
  -e HF_HUB_ETAG_TIMEOUT="${HF_HUB_ETAG_TIMEOUT:-60}" \
  -e HF_HUB_DOWNLOAD_TIMEOUT="${HF_HUB_DOWNLOAD_TIMEOUT:-60}" \
  "${REF}"
# Note: hf-mirror.com's HEAD response is missing HF metadata headers, so
# huggingface_hub raises FileMetadataError and refuses to download; connecting
# directly to huggingface.co (no HF_ENDPOINT) is actually the most reliable
# option — only set HF_ENDPOINT to switch to a mirror on networks where a
# direct connection genuinely doesn't work.

echo "    Waiting for the ML service to be ready ..."
for _ in $(seq 1 60); do
  curl -fsS "http://127.0.0.1:${WARM_PORT}/ping" 2>/dev/null | grep -q pong && break
  sleep 2
done
curl -fsS "http://127.0.0.1:${WARM_PORT}/ping" | grep -q pong || { echo "✗ warm container not ready"; docker logs photos-ml-warm | tail -20; exit 1; }

# A 64x64 white JPEG (generated with ffmpeg and verified to be decodable by
# PIL), used to trigger the model download.
# Note: the previous hand-written 1x1 base64 was truncated, causing immich-ml's
# PIL to raise "image file is truncated" and return 500 — always broke warming.
TEST_JPG="${WARM}/t.jpg"
base64 -d > "${TEST_JPG}" <<'EOF'
/9j/4AAQSkZJRgABAgAAAQABAAD//gAQTGF2YzYxLjE5LjEwMQD/2wBDAAgEBAQEBAUFBQUFBQYG
BgYGBgYGBgYGBgYHBwcICAgHBwcGBgcHCAgICAkJCQgICAgJCQoKCgwMCwsODg4RERT/xABLAAEB
AAAAAAAAAAAAAAAAAAAABwEBAAAAAAAAAAAAAAAAAAAAABABAAAAAAAAAAAAAAAAAAAAABEBAAAA
AAAAAAAAAAAAAAAAAP/AABEIAEAAQAMBIgACEQADEQD/2gAMAwEAAhEDEQA/AL+AAAAAAAAAAAAA
AAAAAAAAAAAAAAAAAAD/2Q==
EOF

warm_predict() {  # $1=description $2=entries $3...=extra -F args
  # Model downloads go over the public network (HF mirror/modelscope); an
  # occasional HEAD failure or dropped connection is normal — retrying within
  # the same container resumes (already-downloaded files stay in the HF
  # cache); only exit if it still fails after 5 retries.
  local desc="$1" entries="$2"; shift 2
  local attempt
  for attempt in 1 2 3 4 5; do
    echo "    Warming ${desc} ... (attempt ${attempt}/5; first run downloads the model, be patient)"
    if curl -fsS --max-time 1800 -X POST "http://127.0.0.1:${WARM_PORT}/predict" \
      -F "entries=${entries}" "$@" > /dev/null; then
      return 0
    fi
    echo "    ${desc} attempt ${attempt} failed, retrying in 30s (resumes from what's already downloaded)"
    sleep 30
  done
  echo "✗ ${desc} warmup failed (5 retries exhausted)"
  docker logs photos-ml-warm 2>&1 | tail -30
  exit 1
}

warm_predict "CLIP visual tower" "{\"clip\":{\"visual\":{\"modelName\":\"${CLIP_MODEL}\"}}}" -F "image=@${TEST_JPG}"
warm_predict "CLIP text tower" "{\"clip\":{\"textual\":{\"modelName\":\"${CLIP_MODEL}\"}}}" -F "text=hello"
warm_predict "face recognition" "{\"facial-recognition\":{\"detection\":{\"modelName\":\"${FACE_MODEL}\"},\"recognition\":{\"modelName\":\"${FACE_MODEL}\"}}}" -F "image=@${TEST_JPG}"
warm_predict "OCR"         "{\"ocr\":{\"detection\":{\"modelName\":\"${OCR_MODEL}\"},\"recognition\":{\"modelName\":\"${OCR_MODEL}\"}}}" -F "image=@${TEST_JPG}"

docker rm -f photos-ml-warm || true
echo "    Model cache $(du -sh "${WARM}/ml-cache" | cut -f1)"

echo "==> [3/4] Packing the model cache bundle ..."
tar -czf "${STAGE}/ml-models.tar.gz" -C "${WARM}" ml-cache

echo "==> [4/4] Assembling distribution bundle ..."
cp "${DEPLOY}/install.sh" "${DEPLOY}/docker-compose.yml" "${STAGE}/"
cp -r "${DEPLOY}/overrides" "${STAGE}/overrides"
printf '%s\n' "${FLAVOR}" > "${STAGE}/FLAVOR"
mkdir -p "${OUT}"
BUNDLE="${OUT}/photos-ml-${FLAVOR}-${IMMICH_VER}.tar.gz"
tar -czf "${BUNDLE}" -C "${STAGE}" .
echo "✓ Done: ${BUNDLE} ($(du -sh "${BUNDLE}" | cut -f1))"
