#!/bin/bash
# DEV-ONLY test harness. NOT part of the shipped release bundle (lives
# outside script.d/ and service.d/, so nothing here is packaged or executed
# by nimoos-update.sh / setup-photos.sh's OS dispatcher).
#
# Exercises, entirely inside a scratch tmp dir with a fake `docker` shim on
# PATH:
#   1. build/scripts/migration/script.d/09-migrate-photos-ml.sh
#   2. the sha256 bundle-verification logic inside
#      build/scripts/setup/service.d/photos/debian/setup-photos.sh
#      (extracted as the `install_photos_ml` function and driven against
#      file:// URLs -- no network, no real OSS).
#
# Never touches /var/lib/nimoos, /DATA/.system_data, /etc/nimoos, systemd, or
# real docker/image state:
#   - the migration script test runs with a fake `docker` on PATH that only
#     appends invocations to a log file and exits 0;
#   - the setup-photos.sh sha256 test runs with a fake `docker` that answers
#     `image inspect` with "not found" (forcing the download path to be
#     exercised) and is otherwise a no-op; the "installer" it downloads is a
#     harmless fake install.sh that writes a marker file, not the real one.
#
# Usage: bash build/scripts/migration/tests/run-tests.sh
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${HERE}/../../../.." && pwd)"
MIGRATION_SCRIPT="${REPO_ROOT}/build/scripts/migration/script.d/09-migrate-photos-ml.sh"
SETUP_SCRIPT="${REPO_ROOT}/build/scripts/setup/service.d/photos/debian/setup-photos.sh"

PASS=0
FAIL=0
ok()  { echo "  OK   $1"; PASS=$((PASS + 1)); }
bad() { echo "  FAIL $1"; FAIL=$((FAIL + 1)); }

SCRATCH="$(mktemp -d "${TMPDIR:-/tmp}/photos-ml-migration-test.XXXXXX")"
trap 'rm -rf "${SCRATCH}"' EXIT

# ---------------------------------------------------------------------------
# Shared bash syntax check first -- cheap, catches typos before anything else.
# ---------------------------------------------------------------------------
echo "== bash -n syntax check =="
SYNTAX_FAIL=0
for f in "${MIGRATION_SCRIPT}" \
         "${REPO_ROOT}/build/scripts/setup/service.d/photos/debian/setup-photos.sh" \
         "${REPO_ROOT}/build/scripts/setup/service.d/photos/arch/setup-photos.sh" \
         "${REPO_ROOT}/script/package-photos-ml.sh" \
         "${HERE}/run-tests.sh"; do
    if bash -n "${f}"; then
        ok "bash -n $(basename "$(dirname "$(dirname "${f}")")")/$(basename "${f}")"
    else
        bad "bash -n FAILED: ${f}"
        SYNTAX_FAIL=1
    fi
done
if diff -q "${REPO_ROOT}/build/scripts/setup/service.d/photos/debian/setup-photos.sh" \
            "${REPO_ROOT}/build/scripts/setup/service.d/photos/arch/setup-photos.sh" >/dev/null; then
    ok "debian/arch setup-photos.sh remain byte-identical"
else
    bad "debian/arch setup-photos.sh DIVERGED"
fi

# ===========================================================================
# Part 1: 09-migrate-photos-ml.sh
# ===========================================================================

FAKE_BIN="${SCRATCH}/bin"
mkdir -p "${FAKE_BIN}"
DOCKER_LOG="${SCRATCH}/docker.log"
: > "${DOCKER_LOG}"
export DOCKER_LOG

cat > "${FAKE_BIN}/docker" <<'SHIM'
#!/bin/bash
# Records every invocation, always succeeds. `info -f ...` intentionally
# prints nothing so the migration script must fall back to its own default.
echo "docker $*" >> "${DOCKER_LOG}"
exit 0
SHIM
chmod +x "${FAKE_BIN}/docker"

LEGACY_COMPOSE='name: nimoos-photos-ml
services:
  immich-machine-learning:
    image: ghcr.io/immich-app/immich-machine-learning:v1.2.3
    restart: unless-stopped
'
MLSERVER_COMPOSE='name: nimoos-photos-ml
services:
  server:
    image: localhost/nimoos-photos-ml:bundled
    restart: unless-stopped
'

setup_ml_cache() {
    local cache_dir="$1"
    rm -rf "${cache_dir}"
    mkdir -p "${cache_dir}/ov-cache"
    echo ov-cache-marker > "${cache_dir}/ov-cache/some-compiled.blob"
    local tree
    for tree in clip facial-recognition ocr; do
        local sub
        for sub in a b; do
            mkdir -p "${cache_dir}/${tree}/model-x/${sub}/openvino"
            echo fake-onnx > "${cache_dir}/${tree}/model-x/${sub}/model.onnx"
            echo fake-compiled > "${cache_dir}/${tree}/model-x/${sub}/openvino/compiled.blob"
        done
    done
}

run_migration() {
    local app_dir="$1" photos_conf="$2" min_free_kb="$3" docker_root="$4"
    APP_DIR="${app_dir}" \
    PHOTOS_ML_MIGRATION_PHOTOS_CONF="${photos_conf}" \
    PHOTOS_ML_MIGRATION_MIN_FREE_KB="${min_free_kb}" \
    PHOTOS_ML_MIGRATION_TMP_DIR="${SCRATCH}" \
    PHOTOS_ML_MIGRATION_DOCKER_ROOT="${docker_root}" \
    PATH="${FAKE_BIN}:${PATH}" \
    bash "${MIGRATION_SCRIPT}"
}

echo "== Test: fresh install (no deployed compose) is a no-op =="
APP_DIR_FRESH="${SCRATCH}/app_dir_fresh"
mkdir -p "${APP_DIR_FRESH}"
: > "${DOCKER_LOG}"
out=$(run_migration "${APP_DIR_FRESH}" "${SCRATCH}/no-photos.conf" 1 "${SCRATCH}")
rc=$?
[ "${rc}" -eq 0 ] && ok "fresh install: exit 0" || bad "fresh install: exit ${rc}"
[ ! -s "${DOCKER_LOG}" ] && ok "fresh install: zero docker invocations" || bad "fresh install: docker invoked: $(cat "${DOCKER_LOG}")"

echo "== Test: legacy stack migration (happy path) =="
APP_DIR="${SCRATCH}/app_dir"
mkdir -p "${APP_DIR}"
printf '%s' "${LEGACY_COMPOSE}" > "${APP_DIR}/docker-compose.yml"
ML_CACHE1="${SCRATCH}/ml-cache-1"
setup_ml_cache "${ML_CACHE1}"
printf 'NIMOOS_PHOTOS_ML_CACHE=%s\n' "${ML_CACHE1}" > "${APP_DIR}/.env"
: > "${DOCKER_LOG}"
out=$(run_migration "${APP_DIR}" "${SCRATCH}/no-photos.conf" 1 "${SCRATCH}")
rc=$?
echo "${out}" | sed 's/^/    /'
[ "${rc}" -eq 0 ] && ok "legacy migration: exit 0" || bad "legacy migration: exit ${rc}"
grep -qF "docker compose -p nimoos-photos-ml -f ${APP_DIR}/docker-compose.yml down" "${DOCKER_LOG}" \
    && ok "legacy migration: compose down recorded" \
    || bad "legacy migration: compose down missing: $(cat "${DOCKER_LOG}")"
grep -qF "docker rmi localhost/nimoos-photos-ml:bundled" "${DOCKER_LOG}" \
    && ok "legacy migration: rmi recorded" \
    || bad "legacy migration: rmi missing: $(cat "${DOCKER_LOG}")"
down_line=$(grep -n "compose -p" "${DOCKER_LOG}" | head -n1 | cut -d: -f1)
rmi_line=$(grep -n "^docker rmi" "${DOCKER_LOG}" | head -n1 | cut -d: -f1)
if [ -n "${down_line}" ] && [ -n "${rmi_line}" ] && [ "${down_line}" -lt "${rmi_line}" ]; then
    ok "legacy migration: down runs before rmi"
else
    bad "legacy migration: down/rmi ordering wrong (down=${down_line} rmi=${rmi_line})"
fi
remaining_ov=$(find "${ML_CACHE1}" -type d -name openvino | wc -l)
[ "${remaining_ov}" -eq 0 ] && ok "legacy migration: openvino dirs removed" || bad "legacy migration: ${remaining_ov} openvino dir(s) remain"
onnx_count=$(find "${ML_CACHE1}" -name model.onnx | wc -l)
[ "${onnx_count}" -eq 6 ] && ok "legacy migration: model.onnx files intact (6)" || bad "legacy migration: model.onnx count=${onnx_count}"
[ -f "${ML_CACHE1}/ov-cache/some-compiled.blob" ] && ok "legacy migration: ov-cache intact" || bad "legacy migration: ov-cache missing"

echo "== Test: idempotent re-entry once compose is mlserver-style (service 'server') =="
printf '%s' "${MLSERVER_COMPOSE}" > "${APP_DIR}/docker-compose.yml"
: > "${DOCKER_LOG}"
out=$(run_migration "${APP_DIR}" "${SCRATCH}/no-photos.conf" 1 "${SCRATCH}")
rc=$?
[ "${rc}" -eq 0 ] && ok "already-mlserver: exit 0" || bad "already-mlserver: exit ${rc}"
[ ! -s "${DOCKER_LOG}" ] && ok "already-mlserver: zero docker invocations" || bad "already-mlserver: docker invoked: $(cat "${DOCKER_LOG}")"

echo "== Test: disk preflight insufficient -> warn, exit 0, no mutation =="
APP_DIR2="${SCRATCH}/app_dir_lowdisk"
mkdir -p "${APP_DIR2}"
printf '%s' "${LEGACY_COMPOSE}" > "${APP_DIR2}/docker-compose.yml"
ML_CACHE2="${SCRATCH}/ml-cache-2"
setup_ml_cache "${ML_CACHE2}"
printf 'NIMOOS_PHOTOS_ML_CACHE=%s\n' "${ML_CACHE2}" > "${APP_DIR2}/.env"
: > "${DOCKER_LOG}"
out=$(run_migration "${APP_DIR2}" "${SCRATCH}/no-photos.conf" 999999999999 "${SCRATCH}")
rc=$?
[ "${rc}" -eq 0 ] && ok "low disk: exit 0 (non-fatal)" || bad "low disk: exit ${rc}"
[ ! -s "${DOCKER_LOG}" ] && ok "low disk: zero docker invocations (preflight ran before teardown)" || bad "low disk: docker invoked: $(cat "${DOCKER_LOG}")"
echo "${out}" | grep -qi "not enough free disk space" && ok "low disk: warning printed" || bad "low disk: no warning in: ${out}"
remaining_ov2=$(find "${ML_CACHE2}" -type d -name openvino | wc -l)
[ "${remaining_ov2}" -eq 6 ] && ok "low disk: openvino dirs left untouched" || bad "low disk: expected 6 untouched openvino dirs, found ${remaining_ov2}"

# ===========================================================================
# Part 2: setup-photos.sh sha256 verification (install_photos_ml)
# ===========================================================================
echo "== Test: setup-photos.sh sha256 verification =="

FUNC_SRC="$(sed -n '/^install_photos_ml() {/,/^}$/p' "${SETUP_SCRIPT}")"
if [ -z "${FUNC_SRC}" ]; then
    bad "could not extract install_photos_ml() from ${SETUP_SCRIPT}"
else
    ok "extracted install_photos_ml() from setup-photos.sh"
fi

FAKE_BIN_NOIMG="${SCRATCH}/bin-noimg"
mkdir -p "${FAKE_BIN_NOIMG}"
cat > "${FAKE_BIN_NOIMG}/docker" <<'SHIM'
#!/bin/bash
# `image inspect` always reports "not found" so the download path is
# exercised regardless of what is actually loaded on the host running this
# test; everything else is a harmless no-op success.
if [[ "${1:-}" == "image" && "${2:-}" == "inspect" ]]; then
    exit 1
fi
exit 0
SHIM
chmod +x "${FAKE_BIN_NOIMG}/docker"

FAKE_INSTALL_MARKER="${SCRATCH}/install-ran"

# Good bundle: a real tar.gz containing a harmless fake install.sh (never the
# real installer -- it must never touch actual docker/system state).
OSS_GOOD="${SCRATCH}/oss-good"
mkdir -p "${OSS_GOOD}"
cat > "${OSS_GOOD}/install.sh" <<EOF
#!/bin/bash
echo fake-install-ran > "${FAKE_INSTALL_MARKER}"
EOF
tar -czf "${OSS_GOOD}/nimoos-photos-ml-2.0.0.tar.gz" -C "${OSS_GOOD}" install.sh
(cd "${OSS_GOOD}" && sha256sum nimoos-photos-ml-2.0.0.tar.gz > nimoos-photos-ml-2.0.0.tar.gz.sha256)

# Corrupted bundle: different bytes, but paired with the GOOD sidecar (as if
# the download got mangled in transit) -- must fail the checksum.
OSS_BAD="${SCRATCH}/oss-bad"
mkdir -p "${OSS_BAD}"
echo "not-a-real-tarball-corrupted-in-transit" > "${OSS_BAD}/nimoos-photos-ml-2.0.0.tar.gz"
cp "${OSS_GOOD}/nimoos-photos-ml-2.0.0.tar.gz.sha256" "${OSS_BAD}/"

# No sidecar at all.
OSS_NOSIDECAR="${SCRATCH}/oss-nosidecar"
mkdir -p "${OSS_NOSIDECAR}"
cp "${OSS_GOOD}/nimoos-photos-ml-2.0.0.tar.gz" "${OSS_NOSIDECAR}/"

run_install_photos_ml() {
    local base_url="$1"
    (
        export PATH="${FAKE_BIN_NOIMG}:${PATH}"
        BUNDLE="nimoos-photos-ml-2.0.0.tar.gz"
        BUNDLE_URL="${base_url}/${BUNDLE}"
        eval "${FUNC_SRC}"
        install_photos_ml
    )
}

rm -f "${FAKE_INSTALL_MARKER}"
out=$(run_install_photos_ml "file://${OSS_GOOD}")
rc=$?
[ "${rc}" -eq 0 ] && ok "good checksum: exits 0" || bad "good checksum: exit ${rc}"
[ -f "${FAKE_INSTALL_MARKER}" ] && ok "good checksum: install.sh ran (bundle accepted)" || bad "good checksum: install.sh did not run: ${out}"

rm -f "${FAKE_INSTALL_MARKER}"
out=$(run_install_photos_ml "file://${OSS_BAD}")
rc=$?
[ "${rc}" -eq 0 ] && ok "corrupted bundle: exits 0 (non-fatal)" || bad "corrupted bundle: exit ${rc}"
[ ! -f "${FAKE_INSTALL_MARKER}" ] && ok "corrupted bundle: install.sh did NOT run" || bad "corrupted bundle: install.sh ran despite bad checksum"
echo "${out}" | grep -qi "checksum mismatch" && ok "corrupted bundle: mismatch warning shown" || bad "corrupted bundle: no mismatch warning: ${out}"

rm -f "${FAKE_INSTALL_MARKER}"
out=$(run_install_photos_ml "file://${OSS_NOSIDECAR}")
rc=$?
[ "${rc}" -eq 0 ] && ok "missing sidecar: exits 0 (non-fatal)" || bad "missing sidecar: exit ${rc}"
[ ! -f "${FAKE_INSTALL_MARKER}" ] && ok "missing sidecar: install.sh did NOT run" || bad "missing sidecar: install.sh ran despite missing sidecar"
echo "${out}" | grep -qi "Failed to download.*\.sha256" && ok "missing sidecar: warning shown" || bad "missing sidecar: no warning: ${out}"

# ===========================================================================
echo
echo "===================================================="
echo " ${PASS} passed, ${FAIL} failed"
echo "===================================================="
[ "${SYNTAX_FAIL}" -eq 0 ] && [ "${FAIL}" -eq 0 ]
