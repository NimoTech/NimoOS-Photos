#!/bin/bash

set -e

readonly CASA_EXEC=nimoos-photos
readonly CASA_SERVICE=nimoos-photos.service
readonly ML_APP_ID=nimoos-photos-ml
readonly ML_APP_DIR=/var/lib/nimoos/apps/${ML_APP_ID}

CASA_SERVICE_PATH=$(systemctl show ${CASA_SERVICE} --no-pager --property FragmentPath | cut -d'=' -sf2)
readonly CASA_SERVICE_PATH

readonly aCOLOUR=(
    '\e[38;5;154m' # green  	| Lines, bullets and separators
    '\e[1m'        # Bold white	| Main descriptions
    '\e[90m'       # Grey		| Credits
    '\e[91m'       # Red		| Update notifications Alert
    '\e[33m'       # Yellow		| Emphasis
)

Show() {
    # OK
    if (($1 == 0)); then
        echo -e "${aCOLOUR[2]}[$COLOUR_RESET${aCOLOUR[0]}  OK  $COLOUR_RESET${aCOLOUR[2]}]$COLOUR_RESET $2"
    # FAILED
    elif (($1 == 1)); then
        echo -e "${aCOLOUR[2]}[$COLOUR_RESET${aCOLOUR[3]}FAILED$COLOUR_RESET${aCOLOUR[2]}]$COLOUR_RESET $2"
    # INFO
    elif (($1 == 2)); then
        echo -e "${aCOLOUR[2]}[$COLOUR_RESET${aCOLOUR[0]} INFO $COLOUR_RESET${aCOLOUR[2]}]$COLOUR_RESET $2"
    # NOTICE
    elif (($1 == 3)); then
        echo -e "${aCOLOUR[2]}[$COLOUR_RESET${aCOLOUR[4]}NOTICE$COLOUR_RESET${aCOLOUR[2]}]$COLOUR_RESET $2"
    fi
}

trap 'onCtrlC' INT
onCtrlC() {
    echo -e "${COLOUR_RESET}"
    exit 1
}

if [[ ! -x "$(command -v ${CASA_EXEC})" ]]; then
    Show 2 "${CASA_EXEC} is not detected, exit the script."
    exit 1
fi

Show 2 "Stopping ${CASA_SERVICE}..."
systemctl disable --now "${CASA_SERVICE}" || Show 3 "Failed to disable ${CASA_SERVICE}"

# Tear down the Photos AI (nimoos-photos-ml-server) system docker app.
if command -v docker >/dev/null 2>&1; then
    Show 2 "Removing Photos AI ML container..."
    docker compose -p "${ML_APP_ID}" -f "${ML_APP_DIR}/docker-compose.yml" down 2>/dev/null || Show 3 "ML compose down failed (non-fatal)."
    docker rmi localhost/nimoos-photos-ml:bundled 2>/dev/null || true
fi

rm -rvf "$(which ${CASA_EXEC})" || Show 3 "Failed to remove ${CASA_EXEC}"
rm -rvf /etc/nimoos/photos.conf || Show 3 "Failed to remove photos.conf"
rm -rvf "${ML_APP_DIR}"
rm -rvf /var/run/nimoos/photos.url

# NOTE: user photo data + ML model cache under /DATA/.system_data/photos are
# intentionally left in place (non-destructive uninstall).
