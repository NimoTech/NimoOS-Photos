#!/bin/bash
# Migrate an existing machine's legacy Photos AI stack (immich-ml) so that the
# v2.0.0 in-house mlserver bundle (see setup-photos.sh) can take its place.
#
# WHY THIS EXISTS
# Machines set up before "Replace immich-ml with in-house inference server"
# (#48) still run the official immich-machine-learning image under the
# nimoos-photos-ml compose project. A plain `nimoos-update.sh` run would never
# touch that stack on its own: setup-photos.sh's idempotent gate only checks
# whether `localhost/nimoos-photos-ml:bundled` is already loaded, and on these
# machines a *different* image occupies that compose project, so nothing would
# ever prompt a switch-over. This script runs as a migration step (BEFORE the
# setup.d pass, see nimoos-update.sh) and does the one-time teardown so the
# normal setup flow can install the new bundle right after, in the same
# update run:
#
#   1. this script tears down the legacy compose stack and deletes the
#      `localhost/nimoos-photos-ml:bundled` image tag (whatever it currently
#      points at -- the legacy stack does not actually use that tag, so this
#      is mostly a defensive no-op, but it also covers a half-migrated box
#      where an old "bundled" tag from a previous attempt is lingering);
#   2. setup-photos.sh's `docker image inspect
#      localhost/nimoos-photos-ml:bundled` gate therefore finds nothing (the
#      image is gone), so it does NOT skip the download -- it fetches and
#      loads the v2.0.0 universal bundle and rewrites
#      ${APP_DIR}/docker-compose.yml to the mlserver-style compose (service
#      `server`), completing the migration.
#
# NO-OP CASES (exit 0, nothing scary printed):
#   - fresh install (no compose ever deployed at APP_DIR)
#   - already migrated (compose already has the mlserver `server` service,
#     not `immich-machine-learning`)
# Both make this script side-effect-free / a pure no-op on the dev machine and
# on any machine that has already gone through the migration once.
#
# IDEMPOTENCY / SAFE RE-ENTRY
# Every mutating step tolerates having already run (compose already down,
# image already removed, openvino/ dirs already gone) by design -- none of
# them are gated on "did the previous step succeed", so a run that was killed
# halfway through (power loss, OOM, etc.) is simply repeated in full next time
# and converges to the same end state. This script must never exit non-zero:
# nimoos-update.sh treats a non-zero migration script exit as FATAL for the
# entire OS update (see its `|| Show 1 "migration failed"`), so every failure
# path here is deliberately soft (warn + continue, or warn + exit 0).
set -uo pipefail
# (Intentionally no `set -e` -- see the exit-0-always contract above.)

readonly APP_ID="nimoos-photos-ml"
APP_DIR="${APP_DIR:-/var/lib/nimoos/apps/${APP_ID}}"
readonly APP_DIR
readonly COMPOSE_FILE="${APP_DIR}/docker-compose.yml"
readonly OVERRIDE_FILE="${APP_DIR}/docker-compose.override.yml"
readonly ENV_FILE="${APP_DIR}/.env"
readonly IMAGE_REF="localhost/nimoos-photos-ml:bundled"

# Overridable only for the test harness (build/scripts/migration/tests/); on a
# real machine these always take their production defaults.
readonly PHOTOS_CONF="${PHOTOS_ML_MIGRATION_PHOTOS_CONF:-/etc/nimoos/photos.conf}"
readonly DEFAULT_PHOTOS_DATA="/DATA/.system_data/photos"
readonly MIN_FREE_KB="${PHOTOS_ML_MIGRATION_MIN_FREE_KB:-10485760}"   # ~10GiB
readonly DOWNLOAD_TMP_DIR="${PHOTOS_ML_MIGRATION_TMP_DIR:-${TMPDIR:-/tmp}}"
readonly DOCKER_ROOT_OVERRIDE="${PHOTOS_ML_MIGRATION_DOCKER_ROOT:-}"

# ── 1) Detect the legacy stack ────────────────────────────────────────────
if [[ ! -f "${COMPOSE_FILE}" ]]; then
    echo "✅ No deployed Photos AI compose at ${APP_DIR}, nothing to migrate."
    exit 0
fi

if ! grep -qE '^[[:space:]]*immich-machine-learning:[[:space:]]*$' "${COMPOSE_FILE}"; then
    echo "✅ Photos AI compose is already on the mlserver stack, nothing to migrate."
    exit 0
fi

echo "🟨 Legacy immich-ml Photos AI stack detected at ${APP_DIR}, migrating to mlserver..."

# ── 2) Disk preflight ──────────────────────────────────────────────────────
# The v2.0.0 bundle setup-photos.sh is about to fetch is a ~4.9GB download
# that gets extracted and docker-loaded into a ~5.1GB image tar. Check there
# is room for that BEFORE tearing down the (working) legacy stack, so a
# disk-starved box keeps a functional Photos AI instead of ending up with
# neither stack. Non-fatal: retried automatically on the next update.
avail_kb() {
    local dir="$1"
    while [[ ! -d "${dir}" && "${dir}" != "/" ]]; do
        dir="$(dirname "${dir}")"
    done
    df -Pk "${dir}" 2>/dev/null | awk 'NR==2{print $4}'
}

docker_root_dir() {
    if [[ -n "${DOCKER_ROOT_OVERRIDE}" ]]; then
        echo "${DOCKER_ROOT_OVERRIDE}"
        return
    fi
    if command -v docker >/dev/null 2>&1; then
        local root
        root="$(docker info -f '{{.DockerRootDir}}' 2>/dev/null || true)"
        [[ -n "${root}" ]] && { echo "${root}"; return; }
    fi
    echo "/var/lib/docker"
}

DOCKER_ROOT="$(docker_root_dir)"
DOCKER_ROOT_AVAIL_KB="$(avail_kb "${DOCKER_ROOT}")"
TMP_AVAIL_KB="$(avail_kb "${DOWNLOAD_TMP_DIR}")"

for label_kb in "docker root (${DOCKER_ROOT}):${DOCKER_ROOT_AVAIL_KB:-0}" "download tmp (${DOWNLOAD_TMP_DIR}):${TMP_AVAIL_KB:-0}"; do
    avail="${label_kb##*:}"
    label="${label_kb%:*}"
    if [[ -z "${avail}" ]] || (( avail < MIN_FREE_KB )); then
        echo "🟨 Not enough free disk space on ${label} (need ~$((MIN_FREE_KB / 1024 / 1024))GiB) to migrate Photos AI to mlserver yet; skipping this run, will retry on the next update."
        exit 0
    fi
done

# ── 3) Tear down the legacy compose stack (tolerate failure) ─────────────
if command -v docker >/dev/null 2>&1; then
    COMPOSE_ARGS=(-f "${COMPOSE_FILE}")
    [[ -f "${OVERRIDE_FILE}" ]] && COMPOSE_ARGS+=(-f "${OVERRIDE_FILE}")
    echo "  -> docker compose -p ${APP_ID} down"
    docker compose -p "${APP_ID}" "${COMPOSE_ARGS[@]}" down \
        || echo "🟨 docker compose down failed (already down?); continuing (non-fatal)."

    # ── 4) Delete the bundled image tag ───────────────────────────────────
    # Immediate delete (not just untag-and-let-it-dangle): this is what opens
    # setup-photos.sh's idempotent gate (`docker image inspect
    # localhost/nimoos-photos-ml:bundled`) so the setup pass later in this
    # same update run actually installs the v2.0.0 bundle instead of skipping.
    echo "  -> docker rmi ${IMAGE_REF}"
    docker rmi "${IMAGE_REF}" \
        || echo "🟨 docker rmi ${IMAGE_REF} failed (already removed?); continuing (non-fatal)."
else
    echo "🟨 docker not found; skipping compose teardown/image removal (non-fatal)."
fi

# ── 5) Clean immich-era OpenVINO compile-cache blobs ──────────────────────
# ONLY the openvino/ compile-cache dirs immich-ml's OpenVINO EP wrote next to
# each model file. Must NOT touch ov-cache/ (mlserver's own OpenVINO EP
# cache), model.onnx / model trees, photos.db, or anything else -- each glob
# below is tight (named subtrees, exact `openvino` leaf) and guarded with
# [[ -d ]] so an empty/unmatched glob can never turn into `rm -rf` on a
# literal glob string or an unintended parent directory.
resolve_ml_cache() {
    local from_env=""
    if [[ -f "${ENV_FILE}" ]]; then
        from_env="$(awk -F= '$1=="NIMOOS_PHOTOS_ML_CACHE"{print $2; exit}' "${ENV_FILE}" 2>/dev/null || true)"
    fi
    if [[ -n "${from_env}" ]]; then
        echo "${from_env}"
        return
    fi
    local conf_data_path photos_data
    conf_data_path="$(awk -F' *= *' '{k=$1; gsub(/^[ \t]+|[ \t]+$/,"",k); if (tolower(k)=="datapath") print $2}' "${PHOTOS_CONF}" 2>/dev/null | tail -n1 || true)"
    photos_data="${conf_data_path:-${DEFAULT_PHOTOS_DATA}}"
    echo "${photos_data}/ml-cache"
}

ML_CACHE="$(resolve_ml_cache)"

if [[ -n "${ML_CACHE}" && -d "${ML_CACHE}" ]]; then
    removed=0
    for tree in clip facial-recognition ocr; do
        [[ -d "${ML_CACHE}/${tree}" ]] || continue
        for ov in "${ML_CACHE}/${tree}"/*/*/openvino; do
            [[ -d "${ov}" ]] || continue
            rm -rf -- "${ov}"
            removed=$((removed + 1))
        done
    done
    echo "✅ Removed ${removed} legacy OpenVINO compile-cache dir(s) under ${ML_CACHE}."
else
    echo "🟨 ml-cache dir not found at ${ML_CACHE:-<unresolved>}; nothing to clean."
fi

echo "✅ Legacy Photos AI (immich-ml) migration step finished; the setup pass will install the v2.0.0 mlserver bundle."
exit 0
