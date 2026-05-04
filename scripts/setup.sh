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

# --- gum bootstrap ------------------------------------------------------------
install_gum() {
    local release_url="https://github.com/charmbracelet/gum/releases"
    local arch_raw arch latest version tarball tmp
    arch_raw="$(uname -m)"
    case "${arch_raw}" in
        x86_64)        arch="x86_64" ;;
        aarch64|arm64) arch="arm64" ;;
        *) echo "unsupported arch: ${arch_raw}" >&2; return 1 ;;
    esac
    latest="$(basename "$(curl -fsSLI -o /dev/null -w '%{url_effective}' "${release_url}/latest")")"
    version="${latest#v}"
    tarball="gum_${version}_Linux_${arch}.tar.gz"
    tmp="$(mktemp -d)"
    trap 'rm -rf "${tmp}"' RETURN
    echo "installing gum ${version} for Linux/${arch}..."
    curl -fsSL -o "${tmp}/gum.tgz" "${release_url}/download/${latest}/${tarball}"
    tar -xzf "${tmp}/gum.tgz" -C "${tmp}"
    sudo install -m 0755 "${tmp}/gum_${version}_Linux_${arch}/gum" /usr/local/bin/gum
    echo "installed: $(/usr/local/bin/gum --version)"
}

if ! command -v gum >/dev/null 2>&1; then
    install_gum || { echo "gum install failed; see https://github.com/charmbracelet/gum" >&2; exit 1; }
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

# --- legacy drop-in cleanup ---------------------------------------------------
# Older versions of this script wrote a drop-in that doesn't reliably override
# the main config's explicit gid = 0. Remove it if present.
if [[ -f "${CONFIG_DROPIN}" ]]; then
    info "removing legacy drop-in ${CONFIG_DROPIN}"
    sudo rm -f "${CONFIG_DROPIN}"
fi

# --- main config: set [grpc].gid in place -------------------------------------
# Drop-in /etc/containerd/config.d/grpc-gid.toml does NOT reliably win against
# an explicit `gid = 0` written by `containerd config default`, so we patch the
# main config in place. Awk handles three cases: (a) gid line exists in [grpc] →
# replace the value; (b) [grpc] section exists without a gid line → insert one;
# (c) no [grpc] section → append the whole section.
if [[ ! -f "${CONFIG_MAIN}" ]]; then
    errlog "${CONFIG_MAIN} does not exist; install containerd first"
    exit 1
fi

CURRENT_GID="$(sudo awk '
    /^[[:space:]]*\[grpc\]/ { in_grpc=1; next }
    /^[[:space:]]*\[/ && !/^[[:space:]]*\[grpc\]/ { in_grpc=0 }
    in_grpc && /^[[:space:]]*gid[[:space:]]*=/ {
        gsub(/[^0-9-]/, "", $0); print; exit
    }
' "${CONFIG_MAIN}")"

if [[ "${CURRENT_GID}" == "${GID}" ]]; then
    info "[grpc].gid is already ${GID} in ${CONFIG_MAIN}"
else
    info "patching ${CONFIG_MAIN} → [grpc].gid = ${GID} (was ${CURRENT_GID:-unset})"
    sudo cp "${CONFIG_MAIN}" "${CONFIG_MAIN}.bak.$(date +%s)"
    TMP="$(mktemp)"
    sudo awk -v gid="${GID}" '
        BEGIN          { in_grpc=0; saw_grpc=0; replaced=0 }
        /^[[:space:]]*\[grpc\]/ {
            in_grpc=1; saw_grpc=1; print; next
        }
        /^[[:space:]]*\[/ && !/^[[:space:]]*\[grpc\]/ {
            if (in_grpc && !replaced) { print "  gid = " gid; replaced=1 }
            in_grpc=0; print; next
        }
        in_grpc && /^[[:space:]]*gid[[:space:]]*=[[:space:]]*-?[0-9]+/ {
            sub(/=[[:space:]]*-?[0-9]+/, "= " gid); replaced=1; print; next
        }
        { print }
        END {
            if (in_grpc && !replaced) { print "  gid = " gid }
            if (!saw_grpc)            { print ""; print "[grpc]"; print "  gid = " gid }
        }
    ' "${CONFIG_MAIN}" > "${TMP}"
    sudo install -m 0644 "${TMP}" "${CONFIG_MAIN}"
    rm -f "${TMP}"
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
    info "(supplementary groups are read at login; a child process cannot mutate the parent's)"
    if gum confirm "Launch \`make run\` now under group ${GROUP}?"; then
        info "exec'ing into: sg ${GROUP} -c 'make run'"
        exec sg "${GROUP}" -c "make run"
    fi
    info "to upgrade your current shell in place, run:  exec newgrp ${GROUP}"
    info "(plain \`newgrp\` nests a child shell; \`exec\` replaces the current shell so it doesn't)"
fi
