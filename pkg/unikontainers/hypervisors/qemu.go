// Copyright (c) 2023-2026, Nubificus LTD
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hypervisors

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
	"golang.org/x/sys/unix"
)

const (
	QemuVmm    VmmType = "qemu"
	QemuBinary string  = "qemu-system-"
)

// Qemu is the single QEMU backend for every platform. The zero value targets
// Linux with KVM; the darwin constructor (NewQemuDarwin) flips the platform
// fields below to target Hypervisor.framework. Keeping one type — rather than
// a parallel QemuDarwin — means both platforms share the command-building
// skeleton and the VMM interface, with the genuinely platform-specific
// choices (accelerator, firmware path, network backend, and darwin's extra
// rootfs realization) localized to explicit branches.
type Qemu struct {
	binaryPath string
	binary     string
	vhost      bool

	// Platform configuration. Zero values reproduce the Linux/KVM backend
	// exactly, so a bare Qemu{} (as constructed by the Linux VMM factory and
	// the unit tests) is unchanged.
	darwin    bool   // Hypervisor.framework target (macOS) vs KVM (Linux)
	accel     string // accelerator flag; "" => "-enable-kvm"
	firmware  string // -L firmware path; "" => "/usr/share/qemu"
	qmpSocket string // QMP control socket; "" => no -qmp
}

func (q *Qemu) Signal(pid int, signal unix.Signal) error {
	return unix.Kill(pid, signal)
}

func (q *Qemu) Stop(pid int) error {
	return killProcess(pid)
}

func (q *Qemu) Ok() error {
	if q.binaryPath == "" {
		return fmt.Errorf("qemu binary path not set")
	}
	return nil
}

// UsesKVM reports whether the monitor uses KVM (Linux) as opposed to HVF.
func (q *Qemu) UsesKVM() bool {
	return !q.darwin
}

// SupportsSharedfs reports monitor support for shared-fs.
func (q *Qemu) SupportsSharedfs(_ string) bool {
	return true
}

func (q *Qemu) Path() string {
	return q.binaryPath
}

func (q *Qemu) accelFlag() string {
	if q.accel != "" {
		return q.accel
	}
	return "-enable-kvm"
}

func (q *Qemu) firmwarePath() string {
	if q.firmware != "" {
		return q.firmware
	}
	return "/usr/share/qemu"
}

// BuildExecCmd builds and validates the QEMU argument vector. The skeleton is
// shared across platforms; the branches guarded by q.darwin cover the
// Hypervisor.framework specifics (HVF accelerator, vmnet/stream networking,
// and the disk-image / directory-rootfs realization that on Linux is handled
// by the orchestration layer instead).
func (q *Qemu) BuildExecCmd(args types.ExecArgs, ukernel types.Unikernel) ([]string, error) {
	qemuMem := BytesToStringMB(args.MemSizeB)
	cmdString := q.binaryPath + " -m " + qemuMem + "M"
	cmdString += " -L " + q.firmwarePath()
	cmdString += " -cpu host"
	cmdString += " " + q.accelFlag()
	cmdString += " -display none -vga none -monitor null"
	// The guest boot cmdline selects the console device: a guest asking
	// for hvc0 gets a virtio console, where console I/O rides virtqueues
	// instead of trapping to the VMM on every UART register access. Any
	// other guest keeps the emulated UART on stdio. signal=off leaves
	// terminal signals (e.g. Ctrl-C) to the guest instead of QEMU.
	if strings.Contains(args.Command, "console=hvc0") {
		cmdString += " -chardev stdio,id=urunc-console,signal=off"
		cmdString += " -device virtio-serial-pci"
		cmdString += " -device virtconsole,chardev=urunc-console"
	} else {
		cmdString += " -serial stdio"
	}

	if args.VCPUs > 0 {
		cmdString += fmt.Sprintf(" -smp %d", args.VCPUs)
	}

	if args.Seccomp {
		cmdString += " --sandbox on"
		cmdString += ",obsolete=deny"
		cmdString += ",elevateprivileges=deny"
		cmdString += ",spawn=deny"
		cmdString += ",resourcecontrol=deny"
	}

	if runtime.GOARCH == "arm64" {
		cmdString += " -M virt"
	}

	// Kernel vs disk image. On darwin a sibling disk.img (built from the
	// container rootfs) boots directly with EDK2 firmware; otherwise boot the
	// kernel. KernelPath (set on darwin) falls back to UnikernelPath so the
	// Linux path is unchanged.
	useDiskImage := false
	kernelPath := args.UnikernelPath
	if args.KernelPath != "" {
		kernelPath = args.KernelPath
	}
	if q.darwin {
		diskImagePath := filepath.Join(filepath.Dir(args.UnikernelPath), "disk.img")
		if _, err := os.Stat(diskImagePath); err == nil {
			cmdString += " -drive format=raw,file=" + diskImagePath + ",if=virtio"
			cmdString += " -bios /opt/homebrew/share/qemu/edk2-aarch64-code.fd"
			useDiskImage = true
		}
	}
	if !useDiskImage {
		cmdString += " -kernel " + kernelPath
	}

	// Networking: platform-specific backend.
	cmdString += q.netCli(args, ukernel)

	// Block devices (shared).
	blockArgs := ukernel.MonitorBlockCli()
	for _, blockArg := range blockArgs {
		blockCli := blockArg.ExactArgs
		if blockCli == "" && blockArg.ID != "" && blockArg.Path != "" {
			blockCli1 := fmt.Sprintf(" -device virtio-blk-pci,serial=%s,drive=%s,scsi=off", blockArg.ID, blockArg.ID)
			blockCli2 := fmt.Sprintf(" -drive format=raw,if=none,id=%s,file=%s", blockArg.ID, blockArg.Path)
			blockCli = blockCli1 + blockCli2
		}
		cmdString += blockCli
	}

	if args.InitrdPath != "" {
		cmdString += " -initrd " + args.InitrdPath
	}

	// Shared filesystem (Linux 9pfs/virtiofs backends).
	switch args.Sharedfs.Type {
	case "9pfs":
		cmdString += " -fsdev local,id=rootfs9p,security_model=none,path=" + args.Sharedfs.Path
		cmdString += " -device virtio-9p-pci,fsdev=rootfs9p,mount_tag=fs0"
	case "virtiofs":
		cmdString += " -object memory-backend-file,id=mem,size=" + qemuMem + "M,mem-path=/tmp,share=on"
		cmdString += " -numa node,memdev=mem"
		cmdString += " -chardev socket,id=char0,path=/tmp/vhostqemu"
		cmdString += " -device vhost-user-fs-pci,queue-size=1024,chardev=char0,tag=fs0"
	default:
		// Nothing to add
	}

	// Darwin realizes the container rootfs as a 9p share (directory) or
	// initrd (file), plus any extra tagged shares. On Linux this is the
	// orchestration layer's job, so it stays behind q.darwin.
	if q.darwin {
		cmdString += q.darwinRootfsArgs(args)
		for _, dir := range args.SharedDirs {
			cmdString += " -fsdev local,id=" + dir.Tag + ",security_model=none,path=" + dir.Path
			cmdString += " -device virtio-9p-pci,fsdev=" + dir.Tag + ",mount_tag=" + dir.Tag
		}
	}

	extraMonArgs := ukernel.MonitorCli()
	if extraInitrd := extraMonArgs.ExtraInitrd; extraInitrd != "" {
		// The unikernel returns the guest-absolute path of the urunit config
		// blob (e.g. "/urunit.conf"). On Linux the reexec'd runtime lives in
		// the container's mount namespace so that path resolves against the
		// rootfs directly. On darwin there is no such namespace, so resolve it
		// against the host rootfs directory where setupUrunitConfig wrote it.
		if q.darwin && filepath.IsAbs(extraInitrd) && args.Sharedfs.Path != "" {
			extraInitrd = filepath.Join(args.Sharedfs.Path, extraInitrd)
		}
		cmdString += " -initrd " + extraInitrd
	}
	cmdString += extraMonArgs.OtherArgs

	if args.VAccelType == "vsock" {
		cmdString += " -device vhost-vsock-pci,id=vhost-vsock-pci0,guest-cid=" + fmt.Sprintf("%d", args.VSockDevID)
	}

	if q.qmpSocket != "" {
		cmdString += " -qmp unix:" + q.qmpSocket + ",server,nowait"
	}

	exArgs := strings.Split(cmdString, " ")
	// A disk image carries its own boot config; only pass -append with a kernel.
	if !useDiskImage {
		exArgs = append(exArgs, "-append", args.Command)
	}
	return exArgs, nil
}

// netCli returns the platform network arguments. The Linux path (tap + vhost)
// is the zero-value default; darwin uses a user-mode-gateway stream netdev or
// vmnet-shared. A unikernel-provided MonitorNetCli overrides both.
func (q *Qemu) netCli(args types.ExecArgs, ukernel types.Unikernel) string {
	if q.darwin {
		if args.Net.UnixSocket != "" {
			return " -netdev stream,id=net0,addr.type=unix,addr.path=" + args.Net.UnixSocket +
				" -device virtio-net-pci,netdev=net0,mac=" + args.Net.MAC
		}
		if args.Net.TapDev != "" {
			if netcli := ukernel.MonitorNetCli(args.Net.TapDev, args.Net.MAC); netcli != "" {
				return netcli
			}
			return " -netdev vmnet-shared,id=net0" +
				" -device virtio-net-pci,netdev=net0,mac=" + args.Net.MAC
		}
		return " -nic none"
	}

	if args.Net.TapDev != "" {
		netcli := ukernel.MonitorNetCli(args.Net.TapDev, args.Net.MAC)
		if netcli == "" {
			netcli += " -netdev tap,id=net0,script=no,downscript=no,ifname="
			netcli += args.Net.TapDev
			if q.vhost {
				netcli += ",vhost=on"
			}
			netcli += fmt.Sprintf(" %s,host_mtu=%d,mac=%s", getVirtioNetArg(), args.Net.MTU, args.Net.MAC)
		}
		return netcli
	}
	return " -nic none"
}

// darwinRootfsArgs shares the container rootfs directory over 9p (mount tag
// "rootfs"), or attaches a rootfs file as initrd. Empty when a block device
// already provides the root.
func (q *Qemu) darwinRootfsArgs(args types.ExecArgs) string {
	if args.BlockDevPath != "" {
		return " -drive format=raw,file=" + args.BlockDevPath + ",if=virtio"
	}
	if args.RootfsPath == "" {
		return ""
	}
	if info, err := os.Stat(args.RootfsPath); err == nil && info.IsDir() {
		return " -fsdev local,id=rootdev,security_model=none,path=" + args.RootfsPath +
			" -device virtio-9p-pci,fsdev=rootdev,mount_tag=rootfs"
	}
	return " -initrd " + args.RootfsPath
}

// PreExec performs pre-execution setup. On darwin it clears any stale QMP
// socket; on Linux QEMU needs nothing.
func (q *Qemu) PreExec(_ types.ExecArgs) error {
	if q.qmpSocket != "" {
		_ = os.Remove(q.qmpSocket)
	}
	return nil
}

// GetQMPSocket returns the QMP control socket path (darwin lifecycle control).
func (q *Qemu) GetQMPSocket() string { return q.qmpSocket }

// SetQMPSocket sets the QMP control socket path.
func (q *Qemu) SetQMPSocket(path string) { q.qmpSocket = path }

// SetBinaryPath overrides the QEMU binary path.
func (q *Qemu) SetBinaryPath(path string) { q.binaryPath = path }

func getVirtioNetArg() string {
	devType := "virtio-net-pci"
	if runtime.GOARCH == "arm64" {
		devType = "virtio-net-device"
	}
	return "-device " + devType + ",netdev=net0"
}
