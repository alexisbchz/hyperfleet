#!/usr/bin/env bash
set -euo pipefail

# Sets up a loopback-backed thin pool for containerd's devmapper snapshotter
# and writes the containerd drop-in config that points at the pool.
# Idempotent: re-running is a no-op if the pool already exists.
#
# Requires: dmsetup, losetup, truncate, modprobe, sudo.

POOL_NAME="${POOL_NAME:-containerd-pool}"
DATA_DIR="${DATA_DIR:-/var/lib/hyperfleet/devmapper}"
DATA_SIZE="${DATA_SIZE:-10G}"
META_SIZE="${META_SIZE:-512M}"
DATA_BLOCK_SIZE="${DATA_BLOCK_SIZE:-128}"     # sectors (= 64 KiB)
LOW_WATER_MARK="${LOW_WATER_MARK:-32768}"     # blocks before pool is "low"
BASE_IMAGE_SIZE="${BASE_IMAGE_SIZE:-8GB}"
CONTAINERD_CONFIG="${CONTAINERD_CONFIG:-/etc/containerd/config.toml}"
CONTAINERD_DROPIN_DIR="${CONTAINERD_DROPIN_DIR:-/etc/containerd/config.d}"
CONTAINERD_DROPIN="${CONTAINERD_DROPIN_DIR}/devmapper.toml"
STATE_FILE="${DATA_DIR}/state.env"
UDEV_RULE="/etc/udev/rules.d/99-hyperfleet-containerd.rules"
ACCESS_GROUP="${ACCESS_GROUP:-containerd}"

require() { command -v "$1" >/dev/null 2>&1 || { echo "missing required tool: $1" >&2; exit 1; }; }
require dmsetup
require losetup
require truncate
require modprobe

# Preflight: kernel modules.
for mod in dm_thin_pool dm_persistent_data dm_bio_prison; do
    if ! grep -q "^${mod}" /proc/modules && ! sudo modprobe "${mod}" 2>/dev/null; then
        # Module may be built into the kernel; check /sys.
        if [[ ! -d "/sys/module/${mod}" ]]; then
            echo "kernel module ${mod} is not loaded and modprobe failed" >&2
            exit 1
        fi
    fi
done

# Idempotent early-exit.
if sudo dmsetup info "${POOL_NAME}" >/dev/null 2>&1; then
    echo "thin pool ${POOL_NAME} already exists; skipping setup"
else
    echo "creating thin pool ${POOL_NAME} in ${DATA_DIR}..."
    sudo mkdir -p "${DATA_DIR}"

    DATA_IMG="${DATA_DIR}/data.img"
    META_IMG="${DATA_DIR}/metadata.img"

    if [[ ! -f "${DATA_IMG}" ]]; then
        sudo truncate -s "${DATA_SIZE}" "${DATA_IMG}"
    fi
    if [[ ! -f "${META_IMG}" ]]; then
        sudo truncate -s "${META_SIZE}" "${META_IMG}"
    fi

    DATA_LOOP="$(sudo losetup --find --show "${DATA_IMG}")"
    META_LOOP="$(sudo losetup --find --show "${META_IMG}")"
    SECTORS="$(sudo blockdev --getsz "${DATA_LOOP}")"

    sudo dmsetup create "${POOL_NAME}" --table \
        "0 ${SECTORS} thin-pool ${META_LOOP} ${DATA_LOOP} ${DATA_BLOCK_SIZE} ${LOW_WATER_MARK} 1 skip_block_zeroing"

    sudo tee "${STATE_FILE}" >/dev/null <<EOF
DATA_IMG=${DATA_IMG}
META_IMG=${META_IMG}
DATA_LOOP=${DATA_LOOP}
META_LOOP=${META_LOOP}
EOF
    echo "thin pool ${POOL_NAME} created (data=${DATA_LOOP}, meta=${META_LOOP})"
fi

# Containerd drop-in.
sudo mkdir -p "${CONTAINERD_DROPIN_DIR}"
sudo tee "${CONTAINERD_DROPIN}" >/dev/null <<EOF
[plugins."io.containerd.snapshotter.v1.devmapper"]
  pool_name = "${POOL_NAME}"
  root_path = "/var/lib/containerd/devmapper"
  base_image_size = "${BASE_IMAGE_SIZE}"
  fs_type = "ext4"
  discard_blocks = true
EOF

# Ensure main config.toml has imports = [...] line; append if missing.
if [[ -f "${CONTAINERD_CONFIG}" ]] && ! sudo grep -q '^imports' "${CONTAINERD_CONFIG}"; then
    echo 'imports = ["/etc/containerd/config.d/*.toml"]' | sudo tee -a "${CONTAINERD_CONFIG}" >/dev/null
fi

# Udev rule: per-VM thin volumes are root:root mode 0660 by default, which means
# Firecracker can't open them as our user. Re-chown to the containerd group.
echo "installing udev rule ${UDEV_RULE}..."
sudo tee "${UDEV_RULE}" >/dev/null <<EOF
SUBSYSTEM=="block", KERNEL=="dm-*", ENV{DM_NAME}=="${POOL_NAME}-snap-*", GROUP="${ACCESS_GROUP}", MODE="0660"
EOF
sudo udevadm control --reload-rules
sudo udevadm trigger --subsystem-match=block --action=change >/dev/null 2>&1 || true

if systemctl is-active --quiet containerd; then
    # Don't restart from inside our own systemd unit (Before=containerd.service
    # would deadlock — systemctl restart blocks until the dependency chain
    # settles, but we ARE the chain). On reboot, hyperfleet-devmapper.service
    # runs ahead of containerd.service so containerd starts fresh after.
    if [[ -z "${INVOCATION_ID:-}" ]]; then
        echo "restarting containerd to pick up devmapper config..."
        sudo systemctl restart containerd
    else
        echo "running under systemd; skipping inline restart (containerd starts after this unit)"
    fi
fi

echo "devmapper setup complete"
