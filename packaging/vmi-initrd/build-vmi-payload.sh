#!/usr/bin/env bash
# Build the host-side VMI telemetry injectables for Option C (zero customer image
# change). Runs on the host (nofire), NOT baked into any customer image.
#
# Produces two artifacts:
#   1. $PAYLOAD/  a payload dir urunc reads at run time and appends onto the
#      per-container initrd copy (AugmentInitrdForVMI). Source file names here
#      MUST match pkg/unikontainers/initrd/vmi.go:vmiPayloadFiles:
#          init  vmi-busybox  vmi-telem-capture  vmi-urunit-agent
#   2. $INITRD  a complete, self-contained cpio(newc) telemetry initrd that urunc
#      stages into the monitor rootfs (com.urunc.vmi.initrd). It is left
#      UNCOMPRESSED so urunc's cpio-append augmentation concatenates cleanly
#      (the kernel initramfs loader walks concatenated archives).
#
# Inputs (override via env):
#   TELEM_CAPTURE  host path to the eBPF rich-capture agent   (default ~/telem-capture)
#   URUNIT_AGENT   host path to the vsock exec/console agent  (default urunc-images dist)
#   BUSYBOX_URL    static busybox with mount/sh/switch_root applets
#
#   TELEM_CAPTURE=/p URUNIT_AGENT=/p ./build-vmi-payload.sh [PAYLOAD_DIR] [INITRD_OUT]
set -euo pipefail

PAYLOAD="${1:-/opt/urunc-vmi/payload}"
INITRD="${2:-/opt/urunc-vmi/vmi-initrd}"
TELEM_CAPTURE="${TELEM_CAPTURE:-$HOME/telem-capture}"
URUNIT_AGENT="${URUNIT_AGENT:-$HOME/urunc-images/images/urunc-agent/dist/urunit-agent}"
BUSYBOX_URL="${BUSYBOX_URL:-https://busybox.net/downloads/binaries/1.35.0-x86_64-linux-musl/busybox}"
here="$(cd "$(dirname "$0")" && pwd)"

command -v cpio >/dev/null || { echo "ERROR: cpio required (apt-get install -y cpio)"; exit 1; }

# ---- 1. stage the payload dir (source names matched to vmi.go) ----
sudo mkdir -p "$PAYLOAD"
sudo install -m0755 "$here/vmi-init" "$PAYLOAD/init"
# NOTE: staged as "busybox" (not "vmi-busybox"): busybox dispatches `busybox
# <applet>` only when argv[0]'s basename is exactly "busybox"; a rename makes
# every invocation exit 127 (applet not found), panicking init.
if [ ! -x "$PAYLOAD/busybox" ]; then
  curl -fsSL "$BUSYBOX_URL" -o /tmp/vmi-busybox && chmod +x /tmp/vmi-busybox
  sudo install -m0755 /tmp/vmi-busybox "$PAYLOAD/busybox"
fi
sudo install -m0755 "$TELEM_CAPTURE" "$PAYLOAD/vmi-telem-capture"
if [ -f "$URUNIT_AGENT" ]; then
  sudo install -m0755 "$URUNIT_AGENT" "$PAYLOAD/vmi-urunit-agent"
else
  echo "WARN: urunit-agent not found at $URUNIT_AGENT"
  echo "      build it: (in urunc-images) images/urunc-agent -> 'make binaries', or"
  echo "      from the urunc darwin/converge fork: go build ./cmd/urunit-agent"
  echo "      (exec/console channel disabled until present; capture still works)"
fi

# ---- 2. assemble the complete, uncompressed telemetry initrd ----
# The in-initrd layout: /init (rdinit), plus the flat /vmi-* helpers.
stage="$(mktemp -d)"
install -m0755 "$PAYLOAD/init"              "$stage/init"
install -m0755 "$PAYLOAD/busybox"           "$stage/busybox"
install -m0755 "$PAYLOAD/vmi-telem-capture" "$stage/vmi-telem-capture"
[ -f "$PAYLOAD/vmi-urunit-agent" ] && install -m0755 "$PAYLOAD/vmi-urunit-agent" "$stage/vmi-urunit-agent"
# minimal mountpoints the injected /init expects
mkdir -p "$stage/proc" "$stage/sys" "$stage/dev" "$stage/newroot" "$stage/var/log"
sudo mkdir -p "$(dirname "$INITRD")"
( cd "$stage" && find . -mindepth 1 -printf '%P\n' | LC_ALL=C sort | cpio -o -H newc --quiet ) | sudo tee "$INITRD" >/dev/null
rm -rf "$stage"

echo "== VMI payload dir: $PAYLOAD =="; sudo ls -la "$PAYLOAD"
echo "== VMI initrd:      $INITRD ($(sudo stat -c%s "$INITRD") bytes, cpio newc, uncompressed) =="
echo
echo "Deploy annotations for a STOCK image (nothing baked in):"
echo "  com.urunc.unikernel.type=linux  com.urunc.unikernel.hypervisor=firecracker"
echo "  com.urunc.unikernel.mountRootfs=true"
echo "  com.urunc.unikernel.binary=/boot/vmlinux   com.urunc.unikernel.initrd=/boot/vmi-initrd"
echo "  com.urunc.vmi.introspect=true"
echo "  com.urunc.vmi.kernel=<host vmlinux>  com.urunc.vmi.initrd=$INITRD  com.urunc.vmi.payload_dir=$PAYLOAD"
