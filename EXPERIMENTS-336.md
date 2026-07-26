# Graceful shutdown of Linux guests — VMM experiments for urunc issue #336

Standalone VMM experiments run on an ARM64 host to resolve unknowns about graceful
shutdown of Linux guests. **No runtime code was modified.** The goal was to answer three
questions with raw command output as evidence:

1. Does Firecracker on aarch64 reject the `SendCtrlAltDel` action, and with what exact error?
2. Does QEMU aarch64 `-M virt` QMP `system_powerdown` deliver a guest-visible event, what
   guest-kernel support is needed (pl061 gpio + gpio-keys), and what happens when the guest
   ignores vs reacts to it?
3. When the guest reacts by calling `reboot(RB_POWER_OFF)`, does QEMU exit cleanly and emit
   the QMP `SHUTDOWN` event with a guest-shutdown cause?

Date of run: **2026-07-26** (UTC). Timeboxed (~60 min); partial where blocked.

---

## TL;DR findings

| # | Question | Answer | Verified here? |
|---|----------|--------|----------------|
| 1 | Firecracker aarch64 rejects `SendCtrlAltDel`? | **Could not run live** — this host has no Firecracker binary, no network to download one, and my user cannot open `/dev/kvm` (`PermissionError [Errno 13]`). Firecracker *cannot* start without KVM. The action is x86-only by design (it drives the i8042 keyboard controller, which does not exist on the `aarch64` machine model). Exact error string **UNVERIFIED** — see Experiment 1. | ❌ blocked |
| 2 | QEMU `system_powerdown` delivers a guest-visible event? | **Yes, but only if the guest kernel binds it.** QEMU always accepts the command (`{"return": {}}`) and emits a host-side `POWERDOWN` QMP event. QEMU's `-M virt` DTB *does* contain both a `pl061` GPIO controller node and a `gpio-keys` power-button node. But turning that into a guest-visible `KEY_POWER` input event requires the guest kernel to have **both `CONFIG_GPIO_PL061` (controller) and `CONFIG_KEYBOARD_GPIO` (the gpio-keys driver)**. With a kernel that has PL061 but *not* gpio-keys, no input device is created, no GPIO IRQ is registered, and the guest never sees the button — QEMU keeps running. | ✅ verified |
| 2b | Guest ignores the event? | QEMU stays alive indefinitely; `system_powerdown` is idempotent (repeated calls each return `{}` and re-emit `POWERDOWN`). | ✅ verified |
| 3 | Guest `reboot(RB_POWER_OFF)` → clean QEMU exit + `SHUTDOWN` event? | **Yes.** Guest prints `reboot: Power down`; QEMU emits `{"event":"SHUTDOWN","data":{"guest":true,"reason":"guest-shutdown"}}` and exits with **code 0** (under `-no-reboot`). On aarch64 `virt` the guest's power-off reaches QEMU via **PSCI `SYSTEM_OFF`**. | ✅ verified |
| 4 | vsock kernel support (stretch) | The kernel used (PBS 6.6.0) has `CONFIG_VSOCKETS=y`, `CONFIG_VIRTIO_VSOCKETS=y`, `CONFIG_VHOST_VSOCK=y`. Firecracker CI / Ubuntu kernels could not be downloaded (no network) so their values are UNVERIFIED. | ⚠️ partial |

**Bottom line for #336:** graceful shutdown of a Linux guest is a two-sided contract.
The VMM has to *ask* (Firecracker: `SendCtrlAltDel` on x86 only, no aarch64 equivalent action;
QEMU: `system_powerdown`) **and** the guest has to be built to *listen* (gpio-keys/ACPI button)
and to *act* (`reboot(RB_POWER_OFF)`). urunc today does neither side — its `Stop()` just
SIGKILLs the VMM process (see Appendix C), which is the gap the issue is about.

---

## Host environment

```
$ uname -a
Linux rp2 6.12.25+rpt-rpi-2712 #1 SMP PREEMPT Debian 1:6.12.25-1+rpt1 (2025-04-30) aarch64 GNU/Linux

$ dpkg --print-architecture
arm64

$ ls -l /dev/kvm
crw-rw---- 1 root kvm 10, 232 Jul 10 19:17 /dev/kvm      # owner root:kvm; my user is not in group kvm

$ id
uid=995(fleet) gid=992(fleet) groups=992(fleet)           # NOT in kvm group

$ python3 -c "import os; os.open('/dev/kvm', os.O_RDWR)"
PermissionError: [Errno 13] Permission denied: '/dev/kvm'  # => no KVM for me; QEMU runs under TCG, FC cannot run
```

Tooling (versions):

| Tool | Present | Version / note |
|------|---------|----------------|
| `qemu-system-aarch64` | ✅ | `QEMU emulator version 9.2.0 (pve-qemu-kvm_9.2.0-2)` |
| `firecracker` | ❌ | not installed; no network to download a release |
| `busybox` | ✅ | `BusyBox v1.35.0 (Debian 1:1.35.0-4+b4)`, **dynamically linked** |
| `curl`,`socat`,`cpio`,`gzip`,`mkfs.ext4` | ✅ | present |
| passwordless `sudo` | ❌ | `sudo: a password is required` — could not install packages or join `kvm` group |
| outbound network | ❌ | all HTTPS/HTTP timed out (S3, github.com, api.github.com, deb.debian.org) |

Because of no network + no `/dev/kvm`, the two downloadable guest kernels from the brief
(Firecracker CI "kernel A" and an Ubuntu generic "kernel B") **could not be obtained**. See
"Kernel selection" for what was used instead.

### Kernel selection

The host `/boot` only contains Raspberry Pi kernels (`kernel8.img`, `kernel_2712.img`); these
have no embedded `IKCONFIG` and are built for Broadcom SoCs, so they are not reliable under
QEMU `-M virt` — **not used**. A suitable full-featured aarch64 kernel *was* found on-disk:

```
/usr/lib/aarch64-linux-gnu/proxmox-backup/file-restore/Image
  -> Linux kernel ARM64 boot executable Image, little-endian, 4K pages
  -> "Linux version 6.6.0-pbs-restore ... #1 SMP Fri Feb  7 08:53:24 UTC 2025"
```

This is Proxmox Backup Server's file-restore kernel, purpose-built to boot under QEMU. Its
config was extracted from the embedded `IKCFG_ST` gzip blob. **This one kernel plays the role
of "kernel B" for every QEMU experiment below.** Relevant config values:

| CONFIG | Value | Meaning for #336 |
|--------|-------|------------------|
| `CONFIG_SERIAL_AMBA_PL011` | `=y` | `console=ttyAMA0` works |
| `CONFIG_VIRTIO`, `CONFIG_VIRTIO_MMIO`, `CONFIG_VIRTIO_BLK` | `=y` | virtio-capable |
| `CONFIG_GPIO_PL061` | `=y` | PL061 GPIO **controller** present |
| **`CONFIG_KEYBOARD_GPIO`** | **`is not set`** | **gpio-keys driver ABSENT** ← the pivotal gap |
| `CONFIG_INPUT_EVDEV` | `=y` | evdev present (so an event device *would* appear if a driver created one) |
| `CONFIG_ACPI`, `CONFIG_ACPI_BUTTON` | `=y` | ACPI power-button path supported (but see Exp 3 note) |
| `CONFIG_VSOCKETS` / `CONFIG_VIRTIO_VSOCKETS` / `CONFIG_VHOST_VSOCK` | `=y` | vsock support (Exp 4) |

Config extraction command:

```
$ python3  # find IKCFG_ST marker in Image, gzip-decompress the blob that follows
IKCFG_ST at 25280648 ; gzip after marker at 25280656 ; wrote 156545 bytes
$ grep -E 'CONFIG_GPIO_PL061|CONFIG_KEYBOARD_GPIO|...' /tmp/pbs-config
CONFIG_ACPI=y
CONFIG_VSOCKETS=y
CONFIG_VIRTIO_VSOCKETS=y
CONFIG_DEVTMPFS=y
CONFIG_INPUT_EVDEV=y
# CONFIG_KEYBOARD_GPIO is not set
CONFIG_SERIAL_AMBA_PL011=y
CONFIG_GPIO_PL061=y
CONFIG_VIRTIO=y
CONFIG_VIRTIO_MMIO=y
CONFIG_ACPI_BUTTON=y
```

### Guest rootfs (initramfs)

Three tiny initramfs images were built with the host's (dynamic) busybox plus the loader and
libc bundled in (`exp336/build.sh`). Each is `cpio -H newc | gzip -9`:

* **ignorer.gz** — mounts `/proc` `/sys`, prints `BOOTED-IGNORER`, then `while true; sleep 1`.
* **reactor.gz** — mounts `/proc` `/sys` `/dev`(devtmpfs), prints `BOOTED-REACTOR`, lists
  `/sys/class/input/event*/device/name`, greps `/proc/interrupts` for PL061, lists the
  `gpio-keys`/`pl061` **device-tree** nodes, then tries to `dd bs=24 count=1` from the
  gpio-keys event device. If none exists it prints `NO-GPIO-KEYS` and falls back to a sleep
  loop. On a successful read it prints `POWER-EVENT-RECEIVED`, `sync`, `poweroff -f`.
* **poweroff.gz** — prints `BOOTED-POWEROFF-NOW`, waits 2 s, then `sync` + `busybox poweroff -f`
  (i.e. `reboot(RB_POWER_OFF)`) to isolate question 3.

Common QEMU invocation (TCG, since no KVM):

```
qemu-system-aarch64 -M virt -cpu max -m 512 \
  -kernel /usr/lib/aarch64-linux-gnu/proxmox-backup/file-restore/Image \
  -initrd <img>.gz -append "console=ttyAMA0 rdinit=/init panic=-1" \
  -no-reboot -display none -serial file:serial-<mode>.log \
  -qmp unix:qmp-<mode>.sock,server,nowait -nic none
```

The kernel boots to `/init` in ~15 s guest-time (~20 s wall under TCG), confirming an FC-style
uncompressed `Image` + initramfs boots fine under `-M virt`.

---

## Experiment 1 — Firecracker aarch64 `SendCtrlAltDel`  ❌ BLOCKED (not executed)

**Hypothesis:** Firecracker rejects `SendCtrlAltDel` on aarch64 because the action drives the
x86 i8042 controller.

**What blocked it (all three independently fatal):**

1. No `firecracker` binary on the host.
2. No outbound network to download a release (`https://github.com/firecracker-microvm/...`
   and the S3 bucket both time out — see host env).
3. Even with a binary, my user cannot open `/dev/kvm` (`PermissionError [Errno 13]`), and
   Firecracker requires KVM to start — so the microVM could never reach `InstanceStart` to
   accept an action over the API socket.

**Therefore the exact HTTP status/body is UNVERIFIED in this environment.** I did not fabricate
a captured value.

**What can still be stated with confidence (design-level, not a live capture):**

* `SendCtrlAltDel` is inherently x86-only: it pulses the i8042 keyboard-controller reset line,
  and Firecracker only instantiates the i8042 device on `x86_64`. The `aarch64` microVM has no
  such device, so there is no equivalent action — graceful-shutdown on FC/aarch64 has to come
  from elsewhere (there is no ACPI and no PSCI-powerdown action exposed by the FC API).
* Consequently, sending `{"action_type":"SendCtrlAltDel"}` to a running aarch64 microVM is
  expected to fail with an HTTP `400 Bad Request` and a `fault_message` to the effect that the
  action is not supported on this architecture. **Exact wording not verified here.**
* Independent of the API, urunc's own Firecracker driver never issues this action anyway; its
  `Stop()` just kills the VMM PID (Appendix C).

**Conclusion:** answer to Q1 is "yes, it is rejected on aarch64 (no i8042)" on design grounds,
but the precise error string must be captured on a host with KVM + a Firecracker binary. This
is the one deliverable this environment could not produce.

---

## Experiment 2 — QEMU `system_powerdown`, guest IGNORES  ✅

Command: `python3 run_exp.py ignore ignorer.gz` → boots `ignorer.gz`, QMP handshake,
`system_powerdown`, watch 35 s, then a second `system_powerdown`.

Raw QMP traffic (trimmed to the relevant lines):

```
{"QMP": {"version": {"qemu": {"micro":0,"minor":2,"major":9}, "package":"pve-qemu-kvm_9.2.0-2"}, "capabilities": []}}
{"return": {}}                                            # qmp_capabilities
{"timestamp": {...}, "event": "POWERDOWN"}                # <-- host-side event, right after 1st system_powerdown
{"return": {}}                                            # system_powerdown #1 accepted
   ... 35s pass, QEMU still running ...
{"timestamp": {...}, "event": "POWERDOWN"}                # <-- 2nd system_powerdown also emits POWERDOWN
{"return": {}}                                            # system_powerdown #2 accepted (idempotent)
>>> after 2nd powerdown, poll=None (None=still running)   # process never exits
```

Guest serial ends at `BOOTED-IGNORER` and nothing further — the guest kernel here has no
gpio-keys driver, so it never even receives the button (same root cause as Exp 3).

| Aspect | Observed |
|--------|----------|
| `system_powerdown` return | `{"return": {}}` (accepted every time) |
| Async event on host | `POWERDOWN` emitted immediately, once per call |
| `SHUTDOWN` event | **none** |
| QEMU exits within 35 s? | **No** (guest ignores) |
| Idempotence | Second call behaves identically; no error |

**Conclusion:** `system_powerdown` is a *request*. QEMU acknowledges it and raises the host-side
`POWERDOWN` event, but if nothing in the guest consumes the button, QEMU runs forever. This is
exactly the "guest ignores it" case and confirms a VMM-only powerdown is not sufficient for a
guaranteed shutdown — it must be paired with a guest that reacts (Exp 3) and, in production, a
timeout-then-kill fallback.

---

## Experiment 3 — QEMU `system_powerdown`, guest REACTS + `reboot(RB_POWER_OFF)`  ✅

### 3a. Does the guest even see the power button? (reactor.gz)

Command: `python3 run_exp.py react reactor.gz`. Guest serial (verbatim, after boot):

```
BOOTED-REACTOR
== input devices ==
                                       <-- EMPTY: no /sys/class/input/event* at all
== /proc/interrupts (pl061/GPIO) ==
no-gpio-irq-line                       <-- PL061 line is not even registered as an IRQ
== DTB gpio-keys node (proves QEMU virt provides it) ==
DT: /sys/firmware/devicetree/base/gpio-keys
DT: /sys/firmware/devicetree/base/pl061@9030000
/sys/firmware/devicetree/base/gpio-keys
GPIO-KEYS-NODE-PRESENT-IN-DTB          <-- QEMU DID describe the button in the device tree
/sys/firmware/devicetree/base/pl061@9030000
NO-GPIO-KEYS                           <-- but no driver bound it -> no input device
REACTOR-WAITING-FALLBACK
```

QMP side (same as Exp 2): `system_powerdown` → `{"return":{}}` + a `POWERDOWN` event, and QEMU
**does not exit** because the guest never receives a `KEY_POWER` event.

This is the key mechanistic result:

| Layer | State with PBS 6.6.0 kernel |
|-------|------------------------------|
| QEMU virt DTB `gpio-keys` node | **present** (`GPIO-KEYS-NODE-PRESENT-IN-DTB`) |
| QEMU virt DTB `pl061@9030000` controller node | **present** |
| Guest `CONFIG_GPIO_PL061` (controller driver) | present (`=y`) |
| Guest `CONFIG_KEYBOARD_GPIO` (gpio-keys driver) | **ABSENT** (`# ... is not set`) |
| `/sys/class/input/event*` | **none created** |
| PL061 GPIO IRQ | **not registered** |
| Guest-visible power event | **never delivered** |

**Conclusion (answers "what guest kernel support is needed"):** on QEMU `-M virt` with a
direct `-kernel` (device-tree) boot, the power button is a GPIO line on the PL061 controller
exposed as a `gpio-keys` DT node. The guest needs **both**: `CONFIG_GPIO_PL061` for the
controller **and** `CONFIG_KEYBOARD_GPIO` for the gpio-keys driver that binds the node and
emits `KEY_POWER` via evdev. PL061 alone (this kernel) is **not enough** — that is precisely
why the reactor sees no input device. A guest built for graceful shutdown under QEMU virt must
enable gpio-keys (or run in ACPI mode with `CONFIG_ACPI_BUTTON`, see note) *and* run something
in userspace (systemd-logind, acpid, or a bespoke reader) that turns `KEY_POWER` into
`reboot(RB_POWER_OFF)`.

> ACPI note: this kernel has `CONFIG_ACPI=y` + `CONFIG_ACPI_BUTTON=y`, but a direct `-kernel`
> boot hands the guest a **device tree**, so the DT/gpio-keys path is what is active, not ACPI.
> Exercising the ACPI GED power-button path would require booting via UEFI (edk2) so the guest
> selects ACPI — not attempted here (no edk2 aarch64 firmware on-disk, no network). **UNVERIFIED.**

### 3b. Guest calls `reboot(RB_POWER_OFF)` directly (poweroff.gz)

Because no locally-available kernel has gpio-keys, the button→userspace→`RB_POWER_OFF` chain
was closed by having the guest invoke `busybox poweroff -f` itself. This isolates and answers
Q3: *given* the guest reacts, does QEMU shut down cleanly?

Command: `python3 run_exp.py poweroff poweroff.gz`. Result:

```
>>> guest reached BOOTED-POWEROFF-NOW
>>> QEMU EXITED after 0.0s, exit_code=0

=== QMP TRAFFIC ===
{"QMP": {"version": {"qemu": {"micro":0,"minor":2,"major":9}, ...}}}
{"return": {}}                                            # qmp_capabilities
{"timestamp": {"seconds":1785057671,"microseconds":979024},
 "event": "SHUTDOWN", "data": {"guest": true, "reason": "guest-shutdown"}}
=== final exit_code=0 ===

=== SERIAL TAIL ===
BOOTED-POWEROFF-NOW
CALLING-RB_POWER_OFF
[   16.771188][  T158] reboot: Power down                # guest kernel powered off
```

| Success criterion | Observed |
|-------------------|----------|
| Guest performs `reboot(RB_POWER_OFF)` | ✅ `reboot: Power down` on serial |
| QEMU emits `SHUTDOWN` event | ✅ `{"event":"SHUTDOWN", ...}` |
| `SHUTDOWN` `guest` field | **`true`** |
| `SHUTDOWN` `reason` field | **`"guest-shutdown"`** (verbatim) |
| QEMU process exits | ✅ |
| Exit code | **`0`** (with `-no-reboot`) |

**Conclusion:** yes — when the guest calls `reboot(RB_POWER_OFF)`, QEMU aarch64 `virt` exits
cleanly with code 0 and emits `SHUTDOWN {"guest": true, "reason": "guest-shutdown"}`. On
aarch64 the guest's power-off is delivered to QEMU via **PSCI `SYSTEM_OFF`** (the `virt`
machine's power controller), not via any x86-style mechanism. A supervising process can watch
for this exact QMP event to distinguish a clean guest-initiated shutdown from a crash/kill.

---

## Experiment 4 — vsock kernel support (stretch)  ⚠️ partial

From the extracted PBS 6.6.0 config (the only kernel obtainable offline):

```
CONFIG_VSOCKETS=y
CONFIG_VIRTIO_VSOCKETS=y
CONFIG_VHOST_VSOCK=y
```

Firecracker CI "kernel A" and an Ubuntu generic "kernel B" could not be downloaded (no
network), so `CONFIG_VIRTIO_VSOCKETS`/`CONFIG_VSOCKETS` for *those* kernels are **UNVERIFIED**.
No live vsock test was requested or run.

---

## Explicit "could not verify" callouts

* **Q1 exact Firecracker error string** — not captured. No FC binary, no network, no
  `/dev/kvm` access. Only a design-level answer is given.
* **Firecracker live behaviour of any kind** — impossible on this host (no KVM for my user).
* **Positive gpio-keys reaction path** (button → evdev `KEY_POWER` → userspace → poweroff) —
  not demonstrated end-to-end because no locally available kernel has `CONFIG_KEYBOARD_GPIO`.
  Each half was shown instead: (a) the button is described in the DTB but not bound without
  gpio-keys (3a); (b) `reboot(RB_POWER_OFF)` → clean `SHUTDOWN` (3b).
* **ACPI GED power-button path** — not exercised (needs UEFI/edk2 boot; not available offline).
* **KVM-accelerated timings** — all QEMU runs are TCG (software) emulation; wall-clock boot
  times are not representative of KVM. Functional/QMP behaviour is unaffected.
* **RPi `/boot` kernels under `-M virt`** — not used (no embedded config, SoC-specific).

---

## Appendix A — exact commands run

```
# environment
uname -a ; dpkg --print-architecture ; ls -l /dev/kvm ; id
python3 -c "import os; os.open('/dev/kvm', os.O_RDWR)"     # -> PermissionError [Errno 13]
qemu-system-aarch64 --version
curl -m20 https://github.com/firecracker-microvm/firecracker/releases   # -> timeout (28)
curl -m20 "https://s3.amazonaws.com/spec.ccfc.min?list-type=2&prefix=firecracker-ci/"  # -> timeout

# kernel config extraction (see run in body)
file /usr/lib/aarch64-linux-gnu/proxmox-backup/file-restore/Image
python3 <extract IKCFG_ST gzip blob> > /tmp/pbs-config

# build initramfs images
bash exp336/build.sh          # -> ignorer.gz reactor.gz poweroff.gz

# experiments (each: boot, QMP handshake, act, observe)
python3 exp336/run_exp.py ignore   exp336/ignorer.gz
python3 exp336/run_exp.py react    exp336/reactor.gz
python3 exp336/run_exp.py poweroff exp336/poweroff.gz
```

Full harness: `exp336/build.sh` (initramfs builder) and `exp336/run_exp.py` (QEMU + QMP
driver) are committed alongside this report. Raw serial logs: `exp336/serial-*.log`.

## Appendix B — reproducing on a KVM-capable x86/aarch64 host (for Q1)

```
# with a firecracker binary and /dev/kvm access:
firecracker --api-sock /tmp/fc.sock &
curl -X PUT --unix-socket /tmp/fc.sock -d '{"kernel_image_path":"vmlinux","boot_args":"console=ttyS0 reboot=k panic=1 pci=off"}' http://localhost/boot-source
curl -X PUT --unix-socket /tmp/fc.sock -d '{"drive_id":"rootfs","path_on_host":"rootfs.ext4","is_root_device":true,"is_read_only":false}' http://localhost/drives/rootfs
curl -X PUT --unix-socket /tmp/fc.sock -d '{"action_type":"InstanceStart"}' http://localhost/actions
# then the action under test:
curl -X PUT --unix-socket /tmp/fc.sock -d '{"action_type":"SendCtrlAltDel"}' http://localhost/actions -w '\n%{http_code}\n'
# expected on aarch64: HTTP 400 + fault_message that the action is unsupported (no i8042)
```

## Appendix C — why this matters for urunc (source references, unmodified)

urunc's hypervisor drivers stop a guest by killing the VMM PID, not by a graceful in-guest
shutdown — the exact gap issue #336 is about:

```
pkg/unikontainers/hypervisors/firecracker.go
  func (fc *Firecracker) Stop(pid int) error { return killProcess(pid) }
pkg/unikontainers/hypervisors/qemu.go
  func (q *Qemu) Stop(pid int) error { return killProcess(pid) }
pkg/unikontainers/hypervisors/utils.go
  func killProcess(pid int) error { ... unix.Kill(pid, unix.SIGKILL) ... }   // SIGKILL, 2s wait
```

A graceful path would instead: (QEMU) issue QMP `system_powerdown` and wait for the
`SHUTDOWN {"guest":true,"reason":"guest-shutdown"}` event before falling back to kill; and
require the guest image to enable gpio-keys (or ACPI button) so it actually reacts.
(Firecracker/aarch64) there is no `SendCtrlAltDel` equivalent, so a cooperative in-guest agent
(e.g. over vsock, which the kernels here support) would be needed instead.
