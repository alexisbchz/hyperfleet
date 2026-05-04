#!/usr/bin/env bash
set -euo pipefail

# Finds the process holding the hyperfleet dev port and offers to stop it.
# Usage: ./scripts/stop.sh [port]   (defaults to 8080)

PORT="${1:-${PORT:-8080}}"

find_pid() {
    if command -v lsof >/dev/null 2>&1; then
        lsof -ti :"$1" 2>/dev/null | head -1
        return
    fi
    if command -v ss >/dev/null 2>&1; then
        sudo ss -ltnp 2>/dev/null \
            | awk -v p=":$1\$" '$4 ~ p { print $NF }' \
            | grep -oE 'pid=[0-9]+' | head -1 | cut -d= -f2
        return
    fi
    if command -v fuser >/dev/null 2>&1; then
        fuser "$1"/tcp 2>/dev/null | tr -s ' ' '\n' | grep -E '^[0-9]+$' | head -1
        return
    fi
    echo "neither lsof, ss, nor fuser found" >&2
    return 1
}

PID="$(find_pid "$PORT" || true)"

if [[ -z "${PID}" ]]; then
    echo "nothing listening on :${PORT}"
    exit 0
fi

echo "PID ${PID} is holding :${PORT}:"
ps -p "${PID}" -o pid,user,etime,cmd 2>&1 | head -2

if command -v gum >/dev/null 2>&1; then
    gum confirm "Kill PID ${PID}?" || { echo "aborted"; exit 0; }
else
    read -r -p "Kill PID ${PID}? [y/N] " ans
    [[ "${ans}" =~ ^[Yy]$ ]] || { echo "aborted"; exit 0; }
fi

kill "${PID}" 2>/dev/null || true
for _ in 1 2 3 4 5; do
    if ! kill -0 "${PID}" 2>/dev/null; then
        echo "stopped"
        exit 0
    fi
    sleep 0.4
done

echo "still alive, sending SIGKILL"
kill -9 "${PID}" 2>/dev/null || true
sleep 0.5
if kill -0 "${PID}" 2>/dev/null; then
    echo "could not stop PID ${PID}" >&2
    exit 1
fi
echo "stopped"
