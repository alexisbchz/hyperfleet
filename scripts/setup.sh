#!/usr/bin/env bash
set -euo pipefail

# Interactive host setup for hyperfleet using charmbracelet/gum.
# Grants the current user permission to talk to containerd via a shared group,
# so the dev server can run unprivileged.
#
# Steps:
#   1. Verify gum + containerd are installed.
#   2. Create a `containerd` group.
#   3. Drop-in /etc/containerd/config.d/grpc-gid.toml setting [grpc].gid.
#   4. Add the current user to the group.
#   5. Restart containerd; verify socket ownership.

GROUP="${HYPERFLEET_GROUP:-containerd}"
USER_NAME="${SUDO_USER:-${USER:-$(id -un)}}"
CONFIG_MAIN="/etc/containerd/config.toml"
CONFIG_DROPIN_DIR="/etc/containerd/config.d"
CONFIG_DROPIN="${CONFIG_DROPIN_DIR}/grpc-gid.toml"
SOCKET="/run/containerd/containerd.sock"

# --- gum check ----------------------------------------------------------------
if ! command -v gum >/dev/null 2>&1; then
    cat <<'MSG' >&2
gum is not installed. Install it first:
  Debian/Ubuntu:  echo 'deb [trusted=yes] https://repo.charm.sh/apt/ /' | sudo tee /etc/apt/sources.list.d/charm.list && sudo apt update && sudo apt install gum
  macOS:          brew install gum
  Go:             go install github.com/charmbracelet/gum@latest
  Releases:       https://github.com/charmbracelet/gum/releases
MSG
    exit 1
fi

header() { gum style --border normal --margin "1 0" --padding "0 2" --foreground 212 "$1"; }
info()   { gum log --time kitchen --level info "$@"; }
warn()   { gum log --time kitchen --level warn "$@"; }
errlog() { gum log --time kitchen --level error "$@"; }
ok()     { gum style --foreground 42 "$1"; }

header "hyperfleet setup"

# --- containerd present? ------------------------------------------------------
if ! command -v containerd >/dev/null 2>&1; then
    errlog "containerd is not installed. Run: make install-containerd"
    exit 1
fi
info "found $(containerd --version)"

# --- group --------------------------------------------------------------------
if getent group "${GROUP}" >/dev/null; then
    info "group ${GROUP} already exists"
else
    gum confirm "Create system group '${GROUP}'?" || { warn "aborted"; exit 1; }
    sudo groupadd --system "${GROUP}"
    info "created group ${GROUP}"
fi
GID="$(getent group "${GROUP}" | cut -d: -f3)"
info "group ${GROUP} → gid ${GID}"

# --- drop-in config -----------------------------------------------------------
sudo mkdir -p "${CONFIG_DROPIN_DIR}"

EXISTING_GID=""
if [[ -f "${CONFIG_DROPIN}" ]]; then
    EXISTING_GID="$(sudo grep -E '^[[:space:]]*gid' "${CONFIG_DROPIN}" | head -1 | grep -oE '[0-9]+' || true)"
fi

if [[ "${EXISTING_GID}" == "${GID}" ]]; then
    info "drop-in already sets grpc.gid = ${GID}"
else
    info "writing ${CONFIG_DROPIN} (grpc.gid = ${GID})"
    sudo tee "${CONFIG_DROPIN}" >/dev/null <<EOF
[grpc]
  gid = ${GID}
EOF
fi

# Ensure the main config imports the drop-in directory.
if [[ -f "${CONFIG_MAIN}" ]] && ! sudo grep -q '^imports' "${CONFIG_MAIN}"; then
    info "adding imports = [\"${CONFIG_DROPIN_DIR}/*.toml\"] to ${CONFIG_MAIN}"
    echo "imports = [\"${CONFIG_DROPIN_DIR}/*.toml\"]" | sudo tee -a "${CONFIG_MAIN}" >/dev/null
fi

# --- user membership ----------------------------------------------------------
if id -nG "${USER_NAME}" | tr ' ' '\n' | grep -qx "${GROUP}"; then
    info "${USER_NAME} already in ${GROUP}"
else
    gum confirm "Add user '${USER_NAME}' to '${GROUP}' group?" || { warn "aborted"; exit 1; }
    sudo usermod -a -G "${GROUP}" "${USER_NAME}"
    info "added ${USER_NAME} to ${GROUP}"
fi

# --- restart containerd -------------------------------------------------------
gum confirm "Restart containerd to apply the new socket gid?" || { warn "aborted before restart"; exit 1; }
gum spin --spinner dot --title "restarting containerd..." -- sudo systemctl restart containerd
sleep 1

# --- verify -------------------------------------------------------------------
if [[ ! -S "${SOCKET}" ]]; then
    errlog "socket ${SOCKET} not present after restart"
    exit 1
fi

SOCK_OWNER="$(stat -c '%U:%G %a' "${SOCKET}")"
info "socket: ${SOCKET} (${SOCK_OWNER})"

EXPECTED_GROUP=$(stat -c '%G' "${SOCKET}")
if [[ "${EXPECTED_GROUP}" != "${GROUP}" ]]; then
    warn "socket group is ${EXPECTED_GROUP}, expected ${GROUP}"
    warn "containerd may have ignored the drop-in; check /etc/containerd/config.toml"
    exit 1
fi

ok "✓ containerd socket is now group-readable as ${GROUP}"

CURRENT_GROUPS="$(id -nG)"
if echo "${CURRENT_GROUPS}" | tr ' ' '\n' | grep -qx "${GROUP}"; then
    ok "✓ your shell session already has the ${GROUP} group"
    info "ready: try \`make run\`"
else
    warn "your current shell does NOT yet have the ${GROUP} group"
    info "either log out and back in, or run: newgrp ${GROUP}"
    info "then: make run"
fi
