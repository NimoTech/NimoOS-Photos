#!/bin/bash
# 打 Photos AI(immich-machine-learning)离线分发包。
# 用法: script/package-photos-ml.sh <openvino|rocm|cpu> [输出目录=./dist]
# 产物: <输出目录>/photos-ml-<flavor>-<IMMICH_VER>.tar.gz
# 需要: docker、curl、外网(ghcr.io + HF(可用 HF_ENDPOINT 指镜像站) + modelscope.cn)
# 注意: rocm 镜像 ≥35GiB,打包机预留足够磁盘;三个模型缓存约 5-7GiB。
set -euo pipefail

FLAVOR="${1:?用法: package-photos-ml.sh <openvino|rocm|cpu> [输出目录]}"
OUT="${2:-./dist}"
IMMICH_VER="v2.7.5"
REF="localhost/nimoos-photos-ml:bundled"
HERE="$(cd "$(dirname "$0")/.." && pwd)"   # 仓库根
DEPLOY="${HERE}/deploy/ml"
WARM_PORT="13003"

case "${FLAVOR}" in
  openvino) TAG="${IMMICH_VER}-openvino" ;;
  rocm)     TAG="${IMMICH_VER}-rocm" ;;
  cpu)      TAG="${IMMICH_VER}" ;;
  *) echo "✗ 未知 flavor: ${FLAVOR}" >&2; exit 1 ;;
esac
SRC="ghcr.io/immich-app/immich-machine-learning:${TAG}"

# 从 Go 常量里抓当前定稿的模型名,保证打包与代码永远一致
CLIP_MODEL="$(grep -oP 'CLIPModelName = "\K[^"]+' "${HERE}/common/constants.go")"
FACE_MODEL="$(grep -oP 'FaceModelName = "\K[^"]+' "${HERE}/common/constants.go")"
OCR_MODEL="$(grep -oP 'OCRModelName  = "\K[^"]+' "${HERE}/common/constants.go")"
[ -n "${CLIP_MODEL}" ] || { echo "✗ 未能从 common/constants.go 抓到 CLIPModelName" >&2; exit 1; }
[ -n "${FACE_MODEL}" ] || { echo "✗ 未能从 common/constants.go 抓到 FaceModelName" >&2; exit 1; }
[ -n "${OCR_MODEL}" ]  || { echo "✗ 未能从 common/constants.go 抓到 OCRModelName" >&2; exit 1; }
echo "==> 模型: clip=${CLIP_MODEL} face=${FACE_MODEL} ocr=${OCR_MODEL}"

STAGE="$(mktemp -d)"
WARM="$(mktemp -d)"
trap 'docker rm -f photos-ml-warm >/dev/null 2>&1 || true; rm -rf "${STAGE}" "${WARM}"' EXIT

echo "==> [1/4] 拉取并重打标签 ${SRC} ..."
docker pull "${SRC}"
docker tag "${SRC}" "${REF}"
docker save -o "${STAGE}/immich-ml.tar" "${REF}"

echo "==> [2/4] 预热模型缓存(临时容器联网下载,CPU 模式即可)..."
mkdir -p "${WARM}/ml-cache"
docker run -d --name photos-ml-warm \
  -p "127.0.0.1:${WARM_PORT}:3003" \
  -v "${WARM}/ml-cache":/cache \
  -e MACHINE_LEARNING_CACHE_FOLDER=/cache \
  ${HF_ENDPOINT:+-e HF_ENDPOINT="${HF_ENDPOINT}"} \
  "${REF}"

echo "    等待 ML 服务就绪..."
for _ in $(seq 1 60); do
  curl -fsS "http://127.0.0.1:${WARM_PORT}/ping" 2>/dev/null | grep -q pong && break
  sleep 2
done
curl -fsS "http://127.0.0.1:${WARM_PORT}/ping" | grep -q pong || { echo "✗ 预热容器未就绪"; docker logs photos-ml-warm | tail -20; exit 1; }

# 1x1 白色 JPEG,用来触发模型下载(内容不重要,能过解码即可)
TEST_JPG="${WARM}/t.jpg"
base64 -d > "${TEST_JPG}" <<'EOF'
/9j/4AAQSkZJRgABAQEASABIAAD/2wBDAP//////////////////////////////////////////
////////////////////////////////////////////////wgALCAABAAEBAREA/8QAFBABAAAA
AAAAAAAAAAAAAAAAAP/aAAgBAQABPxA=
EOF

warm_predict() {  # $1=描述 $2=entries $3...=额外 -F 参数
  local desc="$1" entries="$2"; shift 2
  echo "    预热 ${desc} ...(首次会下载模型,耐心等)"
  curl -fsS --max-time 1800 -X POST "http://127.0.0.1:${WARM_PORT}/predict" \
    -F "entries=${entries}" "$@" > /dev/null \
    || { echo "✗ ${desc} 预热失败"; docker logs photos-ml-warm | tail -30; exit 1; }
}

warm_predict "CLIP 图像塔" "{\"clip\":{\"visual\":{\"modelName\":\"${CLIP_MODEL}\"}}}" -F "image=@${TEST_JPG}"
warm_predict "CLIP 文本塔" "{\"clip\":{\"textual\":{\"modelName\":\"${CLIP_MODEL}\"}}}" -F "text=hello"
warm_predict "人脸"        "{\"facial-recognition\":{\"detection\":{\"modelName\":\"${FACE_MODEL}\"},\"recognition\":{\"modelName\":\"${FACE_MODEL}\"}}}" -F "image=@${TEST_JPG}"
warm_predict "OCR"         "{\"ocr\":{\"detection\":{\"modelName\":\"${OCR_MODEL}\"},\"recognition\":{\"modelName\":\"${OCR_MODEL}\"}}}" -F "image=@${TEST_JPG}"

docker rm -f photos-ml-warm
echo "    模型缓存 $(du -sh "${WARM}/ml-cache" | cut -f1)"

echo "==> [3/4] 打模型缓存包 ..."
tar -czf "${STAGE}/ml-models.tar.gz" -C "${WARM}" ml-cache

echo "==> [4/4] 组装分发包 ..."
cp "${DEPLOY}/install.sh" "${DEPLOY}/docker-compose.yml" "${STAGE}/"
cp -r "${DEPLOY}/overrides" "${STAGE}/overrides"
printf '%s\n' "${FLAVOR}" > "${STAGE}/FLAVOR"
mkdir -p "${OUT}"
BUNDLE="${OUT}/photos-ml-${FLAVOR}-${IMMICH_VER}.tar.gz"
tar -czf "${BUNDLE}" -C "${STAGE}" .
echo "✓ 完成: ${BUNDLE} ($(du -sh "${BUNDLE}" | cut -f1))"
