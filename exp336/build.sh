#!/bin/bash
# Build three busybox initramfs images for QEMU aarch64 shutdown experiments.
set -e
cd "$(dirname "$0")"
ROOT=$(pwd)

build_base() {
  local D="$1"
  rm -rf "$D"; mkdir -p "$D"/{bin,proc,sys,dev,lib,lib/aarch64-linux-gnu}
  cp /usr/bin/busybox "$D/bin/busybox"
  cp /lib/ld-linux-aarch64.so.1 "$D/lib/ld-linux-aarch64.so.1"
  cp /lib/aarch64-linux-gnu/libc.so.6 "$D/lib/aarch64-linux-gnu/libc.so.6"
  cp /lib/aarch64-linux-gnu/libresolv.so.2 "$D/lib/aarch64-linux-gnu/libresolv.so.2"
  for a in sh mount umount sleep echo cat ls poweroff reboot sync head dd printf find grep ln mkdir; do
    ln -sf busybox "$D/bin/$a"
  done
}

pack() {
  local D="$1" OUT="$2"
  ( cd "$D" && find . | cpio -o -H newc 2>/dev/null | gzip -9 ) > "$OUT"
  echo "packed $OUT ($(stat -c%s "$OUT") bytes)"
}

### 1. IGNORER
build_base rootfs-ignorer
cat > rootfs-ignorer/init <<'EOF'
#!/bin/sh
/bin/mount -t proc proc /proc
/bin/mount -t sysfs sys /sys
echo "BOOTED-IGNORER"
while true; do /bin/sleep 1; done
EOF
chmod +x rootfs-ignorer/init
pack rootfs-ignorer ignorer.gz

### 2. REACTOR
build_base rootfs-reactor
cat > rootfs-reactor/init <<'EOF'
#!/bin/sh
/bin/mount -t proc proc /proc
/bin/mount -t sysfs sys /sys
/bin/mount -t devtmpfs dev /dev 2>/dev/null
echo "BOOTED-REACTOR"
echo "== input devices =="
for f in /sys/class/input/event*/device/name; do
  [ -e "$f" ] || continue
  echo "$f = $(cat $f)"
done
echo "== /proc/interrupts (pl061/GPIO) =="
grep -iE "pl061|GPIO" /proc/interrupts || echo "no-gpio-irq-line"
echo "== DTB gpio-keys node (proves QEMU virt provides it) =="
if [ -d /sys/firmware/devicetree/base ]; then
  find /sys/firmware/devicetree/base -iname "*gpio-keys*" -o -iname "*pl061*" 2>/dev/null | while read n; do echo "DT: $n"; done
  ls -d /sys/firmware/devicetree/base/gpio-keys* 2>/dev/null && echo "GPIO-KEYS-NODE-PRESENT-IN-DTB"
  ls -d /sys/firmware/devicetree/base/pl061* 2>/dev/null
else
  echo "no-devicetree (guest may be in ACPI mode)"
fi
# find gpio-keys event device
EV=""
for f in /sys/class/input/event*/device/name; do
  [ -e "$f" ] || continue
  if grep -qi "gpio" "$f"; then
    EV=/dev/input/$(basename $(dirname $(dirname $f)))
  fi
done
if [ -z "$EV" ]; then
  echo "NO-GPIO-KEYS"
  echo "REACTOR-WAITING-FALLBACK"
  while true; do /bin/sleep 1; done
fi
echo "READING-EVENT-DEV $EV"
/bin/dd if="$EV" bs=24 count=1 2>/dev/null
echo "POWER-EVENT-RECEIVED"
/bin/sync
/bin/poweroff -f
EOF
chmod +x rootfs-reactor/init
pack rootfs-reactor reactor.gz

### 3. POWEROFF-NOW (proves reboot(RB_POWER_OFF) -> QMP SHUTDOWN)
build_base rootfs-poweroff
cat > rootfs-poweroff/init <<'EOF'
#!/bin/sh
/bin/mount -t proc proc /proc
/bin/mount -t sysfs sys /sys
echo "BOOTED-POWEROFF-NOW"
/bin/sleep 2
echo "CALLING-RB_POWER_OFF"
/bin/sync
/bin/poweroff -f
echo "SHOULD-NOT-REACH-HERE"
while true; do /bin/sleep 1; done
EOF
chmod +x rootfs-poweroff/init
pack rootfs-poweroff poweroff.gz

echo "ALL-BUILT"
ls -l *.gz
