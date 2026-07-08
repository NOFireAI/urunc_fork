//go:build darwin
// +build darwin

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

	"github.com/sirupsen/logrus"
	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
	"golang.org/x/sys/unix"
)

var hlog = logrus.WithField("subsystem", "hypervisors")

// QemuDarwin implements the VMM interface for QEMU on macOS with HVF acceleration
type QemuDarwin struct {
	binaryPath string
	binary     string
	vhost      bool
	qmpSocket  string // Path to QMP socket for lifecycle management
}

// NewQemuDarwin creates a new QemuDarwin instance for macOS
func NewQemuDarwin(binaryPath string) *QemuDarwin {
	return &QemuDarwin{
		binary:     "qemu-system-aarch64",
		binaryPath: binaryPath,
		vhost:      false,
		qmpSocket:  filepath.Join(os.TempDir(), "urunc-qemu.sock"),
	}
}

// Name returns the name of the VMM
func (q *QemuDarwin) Name() string {
	return "qemu-hvf"
}

// Signal sends a signal to the QEMU process
func (q *QemuDarwin) Signal(pid int, signal unix.Signal) error {
	return unix.Kill(pid, signal)
}

// Stop terminates the QEMU process
func (q *QemuDarwin) Stop(pid int) error {
	return killProcess(pid)
}

// Ok verifies the VMM is properly configured
func (q *QemuDarwin) Ok() error {
	// Verify QEMU binary exists
	if q.binaryPath == "" {
		return fmt.Errorf("qemu binary path not set")
	}
	return nil
}

// UsesKVM returns false on macOS (uses HVF instead)
func (q *QemuDarwin) UsesKVM() bool {
	return false
}

// SupportsSharedfs returns true if QEMU supports shared filesystem
func (q *QemuDarwin) SupportsSharedfs(_ string) bool {
	return true // virtiofs is supported on macOS
}

// Path returns the path to the QEMU binary
func (q *QemuDarwin) Path() string {
	return q.binaryPath
}

// BuildExecCmd builds the QEMU command line for macOS with HVF and vmnet
func (q *QemuDarwin) BuildExecCmd(args types.ExecArgs, ukernel types.Unikernel) ([]string, error) {
	qemuMem := BytesToStringMB(args.MemSizeB)

	// Start building the command
	cmdString := q.binaryPath + " -m " + qemuMem + "M"

	// HVF acceleration (macOS-specific)
	cmdString += " -accel hvf"

	// CPU configuration for Apple Silicon
	cmdString += " -cpu host"

	// Machine type for ARM64
	if runtime.GOARCH == "arm64" {
		cmdString += " -M virt"
		// Ensure QEMU finds firmware/ROM files (needed when binary is copied/re-signed)
		cmdString += " -L /opt/homebrew/share/qemu"
	}

	// Disable graphics and configure serial output
	cmdString += " -display none -vga none -monitor null"

	// Configure serial output
	// Always use -serial stdio first so output can be redirected by the parent process
	// The parent process (run.go) will either display it to terminal (foreground)
	// or redirect it to a file (detached mode)
	cmdString += " -serial stdio"

	// vCPU configuration
	if args.VCPUs > 0 {
		cmdString += fmt.Sprintf(" -smp %d", args.VCPUs)
	}

	// Check if we have a disk image instead of a kernel
	diskImagePath := filepath.Join(filepath.Dir(args.UnikernelPath), "disk.img")
	useDiskImage := false
	if _, err := os.Stat(diskImagePath); err == nil {
		// Boot from disk image
		cmdString += " -drive format=raw,file=" + diskImagePath + ",if=virtio"
		// Add BIOS for ARM64 disk booting
		cmdString += " -bios /opt/homebrew/share/qemu/edk2-aarch64-code.fd"
		useDiskImage = true
	} else {
		// Boot from kernel image
		kernelPath := args.KernelPath
		if kernelPath == "" {
			// Fallback: use "unikernel" as the kernel if KernelPath not specified
			kernelPath = args.UnikernelPath
		}
		cmdString += " -kernel " + kernelPath

		// Block device: attach ext4 image as virtio-blk disk
		if args.BlockDevPath != "" {
			cmdString += " -drive format=raw,file=" + args.BlockDevPath + ",if=virtio"
			hlog.Debugf("Attaching block device: %s", args.BlockDevPath)
		}

		// Check if RootfsPath is a directory (share via 9pfs) or look for initramfs files
		if args.RootfsPath != "" {
			if info, err := os.Stat(args.RootfsPath); err == nil && info.IsDir() {
				// Directory rootfs — share via 9pfs as root filesystem
				cmdString += " -fsdev local,id=rootdev,security_model=none,path=" + args.RootfsPath
				cmdString += " -device virtio-9p-pci,fsdev=rootdev,mount_tag=rootfs"
				hlog.Debugf("Sharing rootfs directory via 9pfs: %s", args.RootfsPath)
			} else {
				// File — use as initrd
				cmdString += " -initrd " + args.RootfsPath
				hlog.Debugf("Using rootfs file as initrd: %s", args.RootfsPath)
			}
		} else if args.BlockDevPath == "" {
			// Fallback: scan for initramfs files by name
			rootfsDir := filepath.Dir(args.UnikernelPath)
			for _, initrdName := range []string{"initramfs", "initramfs.gz", "initrd.gz", "initrd.img"} {
				scanPath := filepath.Join(rootfsDir, initrdName)
				if _, err := os.Stat(scanPath); err == nil {
					cmdString += " -initrd " + scanPath
					hlog.Debugf("Using initramfs: %s", scanPath)
					break
				}
			}
		}
	}

	// Networking: a unix-socket stream netdev (user-mode gateway; needs no
	// entitlements or root) takes precedence over vmnet, which requires the
	// restricted com.apple.vm.networking entitlement.
	if args.Net.UnixSocket != "" {
		cmdString += " -netdev stream,id=net0,addr.type=unix,addr.path=" + args.Net.UnixSocket
		cmdString += " -device virtio-net-pci,netdev=net0,mac=" + args.Net.MAC
	} else if args.Net.TapDev != "" {
		// Use vmnet-shared for NAT mode networking
		netcli := ukernel.MonitorNetCli(args.Net.TapDev, args.Net.MAC)
		if netcli == "" {
			// Default to virtio-net with vmnet-shared backend
			netcli = " -netdev vmnet-shared,id=net0"
			netcli += " -device virtio-net-pci,netdev=net0,mac=" + args.Net.MAC
		}
		cmdString += netcli
	} else {
		cmdString += " -nic none"
	}

	// Block devices
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

	// Initrd support
	if args.InitrdPath != "" {
		cmdString += " -initrd " + args.InitrdPath
	}

	// Shared filesystem (9pfs - no external daemon needed)
	if args.Sharedfs.Type != "" && args.Sharedfs.Path != "" {
		// Use 9pfs for shared directories - works on macOS without virtiofsd daemon
		cmdString += " -fsdev local,id=fs0,security_model=none,path=" + args.Sharedfs.Path
		cmdString += " -device virtio-9p-pci,fsdev=fs0,mount_tag=fs0"
	}

	// Additional tagged shares (one 9p export per directory)
	for _, dir := range args.SharedDirs {
		cmdString += " -fsdev local,id=" + dir.Tag + ",security_model=none,path=" + dir.Path
		cmdString += " -device virtio-9p-pci,fsdev=" + dir.Tag + ",mount_tag=" + dir.Tag
	}

	// Extra monitor arguments
	extraMonArgs := ukernel.MonitorCli()
	if extraMonArgs.ExtraInitrd != "" {
		cmdString += " -initrd " + extraMonArgs.ExtraInitrd
	}
	cmdString += extraMonArgs.OtherArgs

	// vAccel support via vsock
	if args.VAccelType == "vsock" {
		cmdString += " -device vhost-vsock-pci,id=vhost-vsock-pci0,guest-cid=" + fmt.Sprintf("%d", args.VSockDevID)
	}

	// QMP socket for lifecycle management
	qmpDir := filepath.Dir(q.qmpSocket)
	if err := os.MkdirAll(qmpDir, 0700); err != nil {
		hlog.Warnf("failed to create QMP socket directory: %v", err)
	}
	cmdString += " -qmp unix:" + q.qmpSocket + ",server,nowait"

	// Parse the command string into arguments
	exArgs := strings.Split(cmdString, " ")

	// Only add -append if we're using a kernel (not a disk image)
	if !useDiskImage {
		exArgs = append(exArgs, "-append", args.Command)
	}

	hlog.Debugf("QEMU command: %s %s", exArgs[0], strings.Join(exArgs[1:], " "))

	return exArgs, nil
}

// PreExec performs pre-execution setup for QEMU on macOS
func (q *QemuDarwin) PreExec(args types.ExecArgs) error {
	// Clean up any stale QMP socket
	if err := os.Remove(q.qmpSocket); err != nil && !os.IsNotExist(err) {
		hlog.Warnf("failed to remove stale QMP socket: %v", err)
	}
	return nil
}

// GetQMPSocket returns the path to the QMP socket for lifecycle control
func (q *QemuDarwin) GetQMPSocket() string {
	return q.qmpSocket
}

// SetQMPSocket sets the path to the QMP socket for lifecycle control
func (q *QemuDarwin) SetQMPSocket(path string) {
	q.qmpSocket = path
}

// SetBinaryPath overrides the QEMU binary path
func (q *QemuDarwin) SetBinaryPath(path string) {
	q.binaryPath = path
}

// IsHVFAvailable checks if HVF acceleration is available on this system
func IsHVFAvailable() bool {
	// HVF is available on Apple Silicon (arm64) Macs
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return false
	}

	// Additional runtime checks could be added here if needed
	// For now, assume HVF is available on Apple Silicon
	return true
}
