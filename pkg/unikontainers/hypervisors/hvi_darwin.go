//go:build darwin

package hypervisors

import (
	"fmt"
	"os"
	"strconv"

	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
	"golang.org/x/sys/unix"
)

const (
	HviVmm    VmmType = "hvi"
	HviBinary string  = "hvi"
)

// HviDarwin runs an arm64 Linux guest with Hypervisor.framework and HVI's
// in-process virtio devices. Generic container boot exports the unpacked OCI
// directory read-only through HVI's virtio-fs backend; the initrd supplies the
// writable overlay.
type HviDarwin struct {
	binaryPath string
}

func NewHviDarwin(binaryPath string) *HviDarwin {
	return &HviDarwin{binaryPath: binaryPath}
}

func (h *HviDarwin) BuildExecCmd(args types.ExecArgs, _ types.Unikernel) ([]string, error) {
	kernel := args.KernelPath
	if kernel == "" {
		kernel = args.UnikernelPath
	}
	if kernel == "" {
		return nil, fmt.Errorf("hvi: no kernel image to boot")
	}
	mem := DefaultMemory
	if args.MemSizeB != 0 {
		mem = bytesToMiB(args.MemSizeB)
	}
	vcpus := args.VCPUs
	if vcpus == 0 {
		vcpus = 1
	}
	cmd := []string{
		h.Path(), "boot",
		"--kernel", kernel,
		"--mem-mib", strconv.FormatUint(mem, 10),
		"--cpus", strconv.FormatUint(uint64(vcpus), 10),
	}
	if args.InitrdPath != "" {
		cmd = append(cmd, "--initramfs", args.InitrdPath)
	}
	if args.Command != "" {
		cmd = append(cmd, "--cmdline", args.Command)
	}
	if args.BlockDevPath != "" {
		cmd = append(cmd, "--disk", args.BlockDevPath)
	}
	if args.Sharedfs.Path != "" {
		if !args.Sharedfs.ReadOnly {
			return nil, fmt.Errorf("hvi currently supports only read-only virtio-fs exports")
		}
		tag := args.Sharedfs.Tag
		if tag == "" {
			tag = "rootfs"
		}
		cmd = append(cmd, "--share-ro", args.Sharedfs.Path, tag)
	}
	if len(args.SharedDirs) != 0 {
		return nil, fmt.Errorf("hvi currently supports one virtio-fs export; additional shared directories are not yet supported")
	}
	if args.Net.TapDev != "" {
		cmd = append(cmd, "--net")
	}
	if args.AgentSockPath != "" {
		cmd = append(cmd, "--agent-sock", args.AgentSockPath)
	}
	if args.ContainerID != "" {
		cmd = append(cmd, "--sandbox-id", args.ContainerID)
	}
	return cmd, nil
}

func (h *HviDarwin) PreExec(_ types.ExecArgs) error { return nil }

func (h *HviDarwin) Signal(pid int, signal unix.Signal) error {
	return unix.Kill(pid, signal)
}

func (h *HviDarwin) Stop(pid int) error { return killProcess(pid) }

func (h *HviDarwin) Path() string { return h.binaryPath }

func (h *HviDarwin) UsesKVM() bool { return false }

func (h *HviDarwin) SupportsSharedfs(fsType string) bool { return fsType == "virtiofs" }

func (h *HviDarwin) Ok() error {
	info, err := os.Stat(h.binaryPath)
	if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return fmt.Errorf("hvi not found or not executable at %s", h.binaryPath)
	}
	return nil
}
