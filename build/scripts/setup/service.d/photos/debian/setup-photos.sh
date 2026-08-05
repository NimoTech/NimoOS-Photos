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

# ── Photos AI (nimoos-photos-ml-server) offline bundle ────────────────────────
# Download the pinned ML image bundle from OSS and bring up the system docker
# app (docker load -> compose up -> /ping wait, all inside the bundle's install
# .sh). Optional and fully non-fatal: a problem here must never fail the install.
readonly PHOTOS_ML_VERSION="2.0.0"
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
    # Idempotent: if the bundled ML image is already loaded, skip the (~4.9GB)
    # download + load. Prevents re-pulling on every update / repeated setup runs.
    if docker image inspect localhost/nimoos-photos-ml:bundled >/dev/null 2>&1; then
        echo "🟩 Photos AI ML image already present, skipping download."
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
    # Integrity check: the sidecar is uploaded in bare-filename format (see
    # script/package-photos-ml.sh), so `sha256sum -c` works from inside tmp
    # without needing to rewrite the recorded path.
    if ! curl -fL --retry 3 --connect-timeout 10 -o "${tmp}/${BUNDLE}.sha256" "${BUNDLE_URL}.sha256"; then
        echo "🟨 Failed to download ${BUNDLE_URL}.sha256, skipping Photos AI ML."
        rm -rf "${tmp}"
        return 0
    fi
    if ! (cd "${tmp}" && sha256sum -c "${BUNDLE}.sha256" >/dev/null 2>&1); then
        echo "🟨 Checksum mismatch for ${BUNDLE}, skipping Photos AI ML."
        rm -rf "${tmp}"
        return 0
    fi
    if tar -xzf "${tmp}/${BUNDLE}" -C "${tmp}"; then
        # Hard timeout wrapper: if the bundle's own /ping wait hangs, it won't
        # block the whole install (non-fatal either way). 900s (was 300s):
        # docker-loading a ~5.1GB image tar can exceed 300s on slower disks.
        timeout 900 bash "${tmp}/install.sh" \
            || echo "🟨 Photos AI ML installer timed out/errored (non-fatal); retry manually later."
    else
        echo "🟨 Failed to extract ${BUNDLE} (non-fatal)."
    fi
    rm -rf "${tmp}"
}

install_photos_ml || true
