#!/bin/bash
# 离线安装/更新 Photos AI (immich-machine-learning) 系统 app。
# 由 script/package-photos-ml.sh 打出的分发包解压后运行本脚本即可。
# 幂等：重复运行 = 更新到包内镜像版本。
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
APP_ID="nimoos-photos-ml"
APP_DIR="/var/lib/nimoos/apps/${APP_ID}"
ML_CACHE="/DATA/.system_data/photos/ml-cache"
IMAGE_TAR="${HERE}/immich-ml.tar"
IMAGE_REF="localhost/nimoos-photos-ml:bundled"
PING_URL="http://127.0.0.1:3003/ping"
PING_TIMEOUT=120   # 秒

[[ -f "${IMAGE_TAR}" ]] || { echo "✗ 找不到镜像包 ${IMAGE_TAR}" >&2; exit 1; }

echo "==> [1/4] 载入离线镜像 ${IMAGE_REF} ..."
docker load -i "${IMAGE_TAR}"

echo "==> [2/4] 部署 compose 到 ${APP_DIR} ..."
mkdir -p "${APP_DIR}" "${ML_CACHE}"
cp "${HERE}/docker-compose.yml" "${APP_DIR}/docker-compose.yml"

# 项目名固定用 APP_ID，容器名才与其它机器一致（nimoos-photos-ml-immich-machine-learning-1）。
echo "==> [3/4] 启动 ${APP_ID} ..."
docker compose -p "${APP_ID}" -f "${APP_DIR}/docker-compose.yml" up -d

echo "==> [4/4] 等待 ML 就绪（最多 ${PING_TIMEOUT}s，轮询 ${PING_URL}）..."
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
