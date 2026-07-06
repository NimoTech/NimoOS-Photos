#!/bin/bash
# 离线安装/更新 Photos AI (immich-machine-learning) 系统 app。
# 由 script/package-photos-ml.sh 打出的分发包解压后运行本脚本即可。
# 幂等：重复运行 = 更新到包内镜像版本。
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
APP_ID="nimoos-photos-ml"
APP_DIR="/var/lib/nimoos/apps/${APP_ID}"
PHOTOS_DATA="/DATA/.system_data/photos"
ML_CACHE="${PHOTOS_DATA}/ml-cache"
IMAGE_TAR="${HERE}/immich-ml.tar"
MODELS_TAR="${HERE}/ml-models.tar.gz"
IMAGE_REF="localhost/nimoos-photos-ml:bundled"

# ── flavor 与核显自动识别 ────────────────────────────────────────────────
FLAVOR="$(cat "${HERE}/FLAVOR" 2>/dev/null || echo cpu)"

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
echo "==> 分发包 flavor=${FLAVOR},检测到核显厂商=${VENDOR}"
case "${FLAVOR}" in
  openvino)
    [[ "${VENDOR}" == intel ]] || { echo "✗ openvino 包需要 Intel 核显(检测到 ${VENDOR}),请使用对应平台的分发包" >&2; exit 1; } ;;
  rocm)
    [[ "${VENDOR}" == amd ]]   || { echo "✗ rocm 包需要 AMD 核显(检测到 ${VENDOR}),请使用对应平台的分发包" >&2; exit 1; } ;;
  cpu) : ;;
  *) echo "✗ 未知 flavor: ${FLAVOR}" >&2; exit 1 ;;
esac

PING_URL="http://127.0.0.1:3003/ping"
PING_TIMEOUT=120   # 秒
FORCE_MODELS="${FORCE_MODELS:-}"   # =1 强制覆盖已存在的模型缓存

[[ -f "${IMAGE_TAR}" ]] || { echo "✗ 找不到镜像包 ${IMAGE_TAR}" >&2; exit 1; }

echo "==> [1/5] 载入离线镜像 ${IMAGE_REF} ..."
docker load -i "${IMAGE_TAR}"

echo "==> [2/5] 部署 compose 到 ${APP_DIR} ..."
mkdir -p "${APP_DIR}" "${ML_CACHE}"
cp "${HERE}/docker-compose.yml" "${APP_DIR}/docker-compose.yml"

COMPOSE_FILES=(-f "${APP_DIR}/docker-compose.yml")
if [[ "${FLAVOR}" != cpu ]]; then
  cp "${HERE}/overrides/${FLAVOR}.yml" "${APP_DIR}/docker-compose.override.yml"
  COMPOSE_FILES+=(-f "${APP_DIR}/docker-compose.override.yml")
else
  rm -f "${APP_DIR}/docker-compose.override.yml"
fi

# 模型缓存：包内带 ml-models.tar.gz 则铺进 ml-cache，实现首跑零联网。
# 已存在模型则跳过（幂等、再跑很快），FORCE_MODELS=1 可强制覆盖。
echo "==> [3/5] 准备模型缓存 ..."
if [[ -f "${MODELS_TAR}" ]]; then
  if [[ -z "${FORCE_MODELS}" ]] && [[ -d "${ML_CACHE}/clip" ]]; then
    echo "    模型已存在，跳过（FORCE_MODELS=1 可强制覆盖）。"
  else
    echo "    解压模型到 ${ML_CACHE} ..."
    tar -xzf "${MODELS_TAR}" -C "${PHOTOS_DATA}"
    echo "    模型就位（$(du -sh "${ML_CACHE}" 2>/dev/null | cut -f1)）。"
  fi
else
  echo "    本包未带模型（轻量包），首跑将联网下载。"
fi

# 项目名固定用 APP_ID，容器名才与其它机器一致（nimoos-photos-ml-immich-machine-learning-1）。
echo "==> [4/5] 启动 ${APP_ID} ..."
docker compose -p "${APP_ID}" "${COMPOSE_FILES[@]}" up -d

echo "==> [5/5] 等待 ML 就绪（最多 ${PING_TIMEOUT}s，轮询 ${PING_URL}）..."
deadline=$(( SECONDS + PING_TIMEOUT ))
while (( SECONDS < deadline )); do
  if curl -fsS "${PING_URL}" 2>/dev/null | grep -q pong; then
    echo "✓ Photos AI 已就绪并运行中。"
    exit 0
  fi
  sleep 2
done

echo "✗ 超时：ML 未在 ${PING_TIMEOUT}s 内就绪。" >&2
echo "  排查：docker logs ${APP_ID}-immich-machine-learning-1" >&2
exit 1
