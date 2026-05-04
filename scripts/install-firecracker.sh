#!/usr/bin/env bash
set -euo pipefail

# Downloads the latest firecracker release for the host architecture into ./bin.
# Source: https://labs.iximiuz.com/courses/firecracker-hands-on/run-first-microvm#install-firecracker

ARCH="$(uname -m)"
DEST_DIR="${DEST_DIR:-bin}"
RELEASE_URL="https://github.com/firecracker-microvm/firecracker/releases"

mkdir -p "${DEST_DIR}"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

LATEST="$(basename "$(curl -fsSLI -o /dev/null -w '%{url_effective}' "${RELEASE_URL}/latest")")"
echo "Downloading firecracker ${LATEST} for ${ARCH}..."

curl -fsSL "${RELEASE_URL}/download/${LATEST}/firecracker-${LATEST}-${ARCH}.tgz" \
    | tar -xz -C "${TMP_DIR}"

mv "${TMP_DIR}/release-${LATEST}-${ARCH}/firecracker-${LATEST}-${ARCH}" "${DEST_DIR}/firecracker"
chmod +x "${DEST_DIR}/firecracker"

echo "Installed: ${DEST_DIR}/firecracker"
"${DEST_DIR}/firecracker" --version
