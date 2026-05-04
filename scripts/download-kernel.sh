#!/usr/bin/env bash
set -euo pipefail

# Downloads a Firecracker-compatible vmlinux kernel into ./assets.

ASSETS_DIR="${ASSETS_DIR:-assets}"
KERNEL_VERSION="${KERNEL_VERSION:-5.10.225}"
FC_CI_VERSION="${FC_CI_VERSION:-v1.11}"

ARCH="$(uname -m)"
case "${ARCH}" in
    x86_64|aarch64) ;;
    *) echo "unsupported arch: ${ARCH}" >&2; exit 1 ;;
esac

mkdir -p "${ASSETS_DIR}"
KERNEL_PATH="${ASSETS_DIR}/vmlinux"

if [[ -f "${KERNEL_PATH}" ]]; then
    echo "Kernel already exists: ${KERNEL_PATH}"
    exit 0
fi

URL="https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/${FC_CI_VERSION}/${ARCH}/vmlinux-${KERNEL_VERSION}"
echo "Downloading kernel from ${URL}..."
curl -fsSL -o "${KERNEL_PATH}" "${URL}"
echo "Kernel: ${KERNEL_PATH}"
