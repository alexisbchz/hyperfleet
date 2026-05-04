#!/usr/bin/env bash
set -euo pipefail

# Installs containerd from the official GitHub release and registers a systemd unit.
# Requires sudo.

VERSION="${CONTAINERD_VERSION:-2.0.0}"
ARCH="$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')"
URL="https://github.com/containerd/containerd/releases/download/v${VERSION}/containerd-${VERSION}-linux-${ARCH}.tar.gz"
RUNC_VERSION="${RUNC_VERSION:-1.2.3}"
RUNC_URL="https://github.com/opencontainers/runc/releases/download/v${RUNC_VERSION}/runc.${ARCH}"

if command -v containerd >/dev/null 2>&1; then
    echo "containerd already installed: $(containerd --version)"
    exit 0
fi

TMP="$(mktemp -d)"; trap 'rm -rf "${TMP}"' EXIT
echo "Downloading containerd ${VERSION}..."
curl -fsSL -o "${TMP}/containerd.tgz" "${URL}"
sudo tar -C /usr/local -xzf "${TMP}/containerd.tgz"

echo "Downloading runc ${RUNC_VERSION}..."
curl -fsSL -o "${TMP}/runc" "${RUNC_URL}"
sudo install -m 0755 "${TMP}/runc" /usr/local/sbin/runc

sudo mkdir -p /usr/local/lib/systemd/system /etc/containerd
curl -fsSL "https://raw.githubusercontent.com/containerd/containerd/v${VERSION}/containerd.service" \
    | sudo tee /usr/local/lib/systemd/system/containerd.service >/dev/null

containerd config default | sudo tee /etc/containerd/config.toml >/dev/null

sudo systemctl daemon-reload
sudo systemctl enable --now containerd

echo "Installed: $(containerd --version)"
