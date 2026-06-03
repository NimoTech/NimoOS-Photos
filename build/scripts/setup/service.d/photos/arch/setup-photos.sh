#!/bin/bash

set -e

## base variables
readonly APP_NAME="nimoos-photos"
readonly APP_NAME_SHORT="photos"

# copy config file from sample if absent
readonly CONF_PATH=/etc/nimoos
readonly CONF_FILE=${CONF_PATH}/${APP_NAME_SHORT}.conf
readonly CONF_FILE_SAMPLE=${CONF_PATH}/${APP_NAME_SHORT}.conf.sample

if [ ! -f "${CONF_FILE}" ]; then
    echo "Initializing config file..."
    cp -v "${CONF_FILE_SAMPLE}" "${CONF_FILE}"
fi

# enable service (start happens in the installer's service loop)
systemctl daemon-reload

echo "Enabling service..."
systemctl enable --force --no-ask-password "${APP_NAME}.service"

# ── Photos AI (immich-machine-learning) offline bundle ────────────────────────
# Download the pinned ML image bundle from OSS and bring up the system docker
# app (docker load -> compose up -> /ping wait, all inside the bundle's install
# .sh). Optional and fully non-fatal: a problem here must never fail the install.
readonly PHOTOS_ML_VERSION="1.0.1"
readonly OSS_DOMAIN="https://nimoos.oss-cn-shenzhen.aliyuncs.com"
readonly BUNDLE="nimoos-photos-ml-${PHOTOS_ML_VERSION}.tar.gz"
readonly BUNDLE_URL="${OSS_DOMAIN}/NimoTech/NimoOS-Photos/releases/download/photos-ml-${PHOTOS_ML_VERSION}/${BUNDLE}"

install_photos_ml() {
    local arch
    arch="$(uname -m)"
    if [[ "${arch}" != "x86_64" && "${arch}" != "amd64" ]]; then
        echo "🟨 No Photos AI ML bundle for ${arch} yet, skipping."
        return 0
    fi
    if ! command -v docker >/dev/null 2>&1; then
        echo "🟨 docker not found, skipping Photos AI ML setup."
        return 0
    fi
    local tmp
    tmp="$(mktemp -d)"
    echo "  -> Downloading ${BUNDLE} ..."
    if ! curl -fL --retry 3 --connect-timeout 10 -o "${tmp}/${BUNDLE}" "${BUNDLE_URL}"; then
        echo "🟨 Failed to download ${BUNDLE_URL}, skipping Photos AI ML."
        rm -rf "${tmp}"
        return 0
    fi
    if tar -xzf "${tmp}/${BUNDLE}" -C "${tmp}"; then
        # timeout 硬包住:bundle 内的 /ping 等待若挂死也不会阻塞整个安装(非致命)
        timeout 300 bash "${tmp}/install.sh" \
            || echo "🟨 Photos AI ML installer timed out/errored (non-fatal); 可稍后手动重试。"
    else
        echo "🟨 Failed to extract ${BUNDLE} (non-fatal)."
    fi
    rm -rf "${tmp}"
}

install_photos_ml || true
