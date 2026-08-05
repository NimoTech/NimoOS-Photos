#!/bin/bash
# Build the Photos AI (NimoOS in-house mlserver) offline distribution bundle.
# Usage: script/package-photos-ml.sh [version=1.0.0] [output dir=./dist]
# Output: <output dir>/photos-ml-universal-v<version>.tar.gz (+ .sha256)
# Requires: docker (buildkit), and either `uv` or a python3 with
# huggingface_hub==1.26.0 + rapidocr==3.6.0 installed (for download_models.py),
# and internet access (huggingface.co + modelscope.cn).
#
# One image now covers every supported device (CPU + Intel iGPU via OpenVINO
# EP -- see mlserver/Dockerfile), so there is no more per-flavor pull/retag
# and no more "warm a temp container with fake predict requests to trigger a
# model download" dance: the image is built directly from source and the
# models are fetched straight from their upstream repos by download_models.py.
set -euo pipefail

VERSION="${1:-1.0.0}"
OUT="${2:-./dist}"
REF="localhost/nimoos-photos-ml:bundled"
HERE="$(cd "$(dirname "$0")/.." && pwd)"   # repo root
MLSERVER_DIR="${HERE}/mlserver"
DEPLOY="${HERE}/deploy/ml"

# Pull the currently pinned model names from the Go constants, so the bundle
# and the code always stay in sync.
CLIP_MODEL="$(grep -oP 'CLIPModelName = "\K[^"]+' "${HERE}/common/constants.go")"
FACE_MODEL="$(grep -oP 'FaceModelName = "\K[^"]+' "${HERE}/common/constants.go")"
OCR_MODEL="$(grep -oP 'OCRModelName  = "\K[^"]+' "${HERE}/common/constants.go")"
[ -n "${CLIP_MODEL}" ] || { echo "✗ failed to read CLIPModelName from common/constants.go" >&2; exit 1; }
[ -n "${FACE_MODEL}" ] || { echo "✗ failed to read FaceModelName from common/constants.go" >&2; exit 1; }
[ -n "${OCR_MODEL}" ]  || { echo "✗ failed to read OCRModelName from common/constants.go" >&2; exit 1; }
echo "==> Models: clip=${CLIP_MODEL} face=${FACE_MODEL} ocr=${OCR_MODEL}"

# download_models.py carries its own PEP 723 inline dependency list, so `uv
# run` needs nothing pre-installed; fall back to plain python3 (whoever runs
# this script is then responsible for having huggingface_hub/rapidocr on it).
if command -v uv >/dev/null 2>&1; then
  DOWNLOAD_CMD=(uv run "${MLSERVER_DIR}/golden/download_models.py")
else
  echo "    (uv not found, falling back to python3 -- it must already have" >&2
  echo "     huggingface_hub==1.26.0 and rapidocr==3.6.0 installed)" >&2
  DOWNLOAD_CMD=(python3 "${MLSERVER_DIR}/golden/download_models.py")
fi

STAGE="$(mktemp -d)"
# Raw model download dir: by default a temp dir removed when done; set
# PHOTOS_ML_MODELS_DIR to a persistent directory so a failed/interrupted
# multi-GB download can resume instead of starting over (download_models.py
# skips files that already exist with the right sha256).
if [[ -n "${PHOTOS_ML_MODELS_DIR:-}" ]]; then
  MODELS_DIR="${PHOTOS_ML_MODELS_DIR}"
  mkdir -p "${MODELS_DIR}"
  KEEP_MODELS_DIR=1
else
  MODELS_DIR="$(mktemp -d)"
  KEEP_MODELS_DIR=""
fi
trap 'rm -rf "${STAGE}"; [[ -z "${KEEP_MODELS_DIR}" ]] && rm -rf "${MODELS_DIR}"' EXIT

echo "==> [1/4] Building ${REF} from mlserver/ ..."
DOCKER_BUILDKIT=1 docker build -t "${REF}" "${MLSERVER_DIR}"

echo "==> [2/4] Saving image to immich-ml.tar ..."
# Filename kept as immich-ml.tar (not renamed) -- deploy/ml/install.sh looks
# for this exact name.
docker save -o "${STAGE}/immich-ml.tar" "${REF}"

echo "==> [3/4] Downloading models and packing ml-models.tar.gz ..."
"${DOWNLOAD_CMD[@]}" \
  --out "${MODELS_DIR}/ml-cache" \
  --clip-model "${CLIP_MODEL}" \
  --face-model "${FACE_MODEL}" \
  --ocr-model "${OCR_MODEL}"
tar -czf "${STAGE}/ml-models.tar.gz" -C "${MODELS_DIR}" ml-cache

echo "==> [4/4] Assembling distribution bundle ..."
cp "${DEPLOY}/install.sh" "${DEPLOY}/docker-compose.yml" "${STAGE}/"
cp -r "${DEPLOY}/overrides" "${STAGE}/overrides"
# Single universal image/bundle now -- no more openvino/rocm/cpu split.
printf 'universal\n' > "${STAGE}/FLAVOR"
mkdir -p "${OUT}"
BUNDLE="${OUT}/photos-ml-universal-v${VERSION}.tar.gz"
tar -czf "${BUNDLE}" -C "${STAGE}" .
# Bare-filename format (not the absolute/relative path sha256sum records by
# default): setup-photos.sh downloads bundle + sidecar into the same tmp dir
# and runs `sha256sum -c` from inside it, which requires the recorded name to
# match the file sitting right next to it, not a path from this machine.
(cd "$(dirname "${BUNDLE}")" && sha256sum "$(basename "${BUNDLE}")" > "$(basename "${BUNDLE}")".sha256)
echo "✓ Done: ${BUNDLE} ($(du -sh "${BUNDLE}" | cut -f1))"

# This script's local dist filename (photos-ml-universal-v<version>.tar.gz)
# is not what setup-photos.sh downloads (nimoos-photos-ml-<version>.tar.gz) --
# nothing renames it automatically, so print the exact upload commands
# (rename-on-upload) instead of leaving that as tribal knowledge.
UPLOAD_NAME="nimoos-photos-ml-${VERSION}.tar.gz"
UPLOAD_DIR="NimoTech/NimoOS-Photos/releases/download/photos-ml-${VERSION}"
UPLOAD_SIDECAR="${OUT}/${UPLOAD_NAME}.sha256"
echo ""
echo "  setup-photos.sh expects the OSS object named '${UPLOAD_NAME}', not the"
echo "  local '$(basename "${BUNDLE}")' -- rename on upload:"
echo ""
echo "  Upload with:"
echo "    ossutil cp ${BUNDLE} oss://nimoos/${UPLOAD_DIR}/${UPLOAD_NAME} -f"
echo ""
echo "  The sidecar must be regenerated against the renamed bare filename (its"
echo "  recorded name has to match '${UPLOAD_NAME}', not '$(basename "${BUNDLE}")'"
echo "  -- sha256sum -c on the downloading machine matches by name):"
echo "    sha256sum ${BUNDLE} | awk -v n=\"${UPLOAD_NAME}\" '{print \$1\"  \"n}' > ${UPLOAD_SIDECAR}"
echo "    ossutil cp ${UPLOAD_SIDECAR} oss://nimoos/${UPLOAD_DIR}/${UPLOAD_NAME}.sha256 -f"
