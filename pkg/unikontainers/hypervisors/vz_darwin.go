//go:build darwin

package hypervisors

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
	"golang.org/x/sys/unix"
)

const VzVmm VmmType = "vz"

// VzDarwin implements the VMM interface for Apple Virtualization.framework
type VzDarwin struct {
	vzRunnerPath string
	qmpSocket    string
}

// NewVzDarwin creates a new Vz VMM instance
func NewVzDarwin() *VzDarwin {
	// Find vz-runner in the same directory as urunc-macos
	exePath, err := os.Executable()
	if err != nil {
		exePath = "./vz-runner"
	} else {
		exePath = filepath.Join(filepath.Dir(exePath), "vz-runner")
	}

	return &VzDarwin{
		vzRunnerPath: exePath,
	}
}

// BuildExecCmd builds the command to launch the VM via vz-runner
func (v *VzDarwin) BuildExecCmd(args types.ExecArgs, ukernel types.Unikernel) ([]string, error) {
	if args.KernelPath == "" {
		return nil, fmt.Errorf("kernel path required for Vz backend")
	}

	cmdArgs := []string{
		v.vzRunnerPath,
		"--kernel", args.KernelPath,
		"--mem", fmt.Sprintf("%d", args.MemSizeB/(1024*1024)),
		"--cpus", fmt.Sprintf("%d", args.VCPUs),
	}

	// Add initrd if provided
	if args.InitrdPath != "" {
		cmdArgs = append(cmdArgs, "--initrd", args.InitrdPath)
	}

	// Add kernel cmdline
	if args.Command != "" {
		cmdArgs = append(cmdArgs, "--cmdline", args.Command)
	}

	// Deterministic MAC address for the NAT network device, so the guest's
	// DHCP lease (and therefore its IP) can be found on the host by MAC.
	if args.Net.MAC != "" {
		cmdArgs = append(cmdArgs, "--mac", args.Net.MAC)
	}

	// Block device: attach ext4 image as virtio-blk disk
	if args.BlockDevPath != "" {
		cmdArgs = append(cmdArgs, "--rootfs", args.BlockDevPath)
	} else if args.RootfsPath != "" && args.InitrdPath == "" {
		// If rootfs directory exists and no initrd, share rootfs via virtiofs
		// The kernel will mount it as root with root=rootfs rootfstype=virtiofs
		info, err := os.Stat(args.RootfsPath)
		if err == nil && info.IsDir() {
			cmdArgs = append(cmdArgs, "--share", args.RootfsPath, "rootfs")
		}
	}

	// Add user-requested shared directory via virtiofs
	if args.Sharedfs.Type != "" && args.Sharedfs.Path != "" {
		cmdArgs = append(cmdArgs, "--share", args.Sharedfs.Path, "shared")
	}

	return cmdArgs, nil
}

// PreExec performs any pre-execution setup
func (v *VzDarwin) PreExec(args types.ExecArgs) error {
	// Create QMP socket directory if needed
	if v.qmpSocket != "" {
		sockDir := filepath.Dir(v.qmpSocket)
		if err := os.MkdirAll(sockDir, 0700); err != nil {
			return fmt.Errorf("failed to create QMP socket directory: %w", err)
		}
	}
	return nil
}

// Signal sends a signal to the vz-runner process
func (v *VzDarwin) Signal(pid int, signal unix.Signal) error {
	return unix.Kill(pid, signal)
}

// Stop stops the VM
func (v *VzDarwin) Stop(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}

	// Try graceful shutdown via SIGTERM first
	// The vz-runner process has signal handlers that will gracefully stop the VM
	if err := proc.Signal(os.Interrupt); err != nil {
		// If SIGTERM fails, fall back to SIGKILL
		return proc.Kill()
	}

	// Give the process time to shut down gracefully (5 seconds)
	// This is a simplified timeout; could be enhanced with polling
	done := make(chan error, 1)
	go func() {
		_, err := proc.Wait()
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil && err.Error() != "waitid: no child processes" {
			return err
		}
		return nil
	case <-time.After(5 * time.Second):
		// If still running after 5 seconds, force kill
		return proc.Kill()
	}
}

// Path returns the path to the vz-runner binary
func (v *VzDarwin) Path() string {
	return v.vzRunnerPath
}

// UsesKVM returns false (Vz uses HVF, not KVM)
func (v *VzDarwin) UsesKVM() bool {
	return false
}

// SupportsSharedfs returns true (Vz supports directory sharing)
func (v *VzDarwin) SupportsSharedfs(_ string) bool {
	return true
}

// Ok checks if the Vz backend is available
func (v *VzDarwin) Ok() error {
	// Check if vz-runner exists and is executable
	info, err := os.Stat(v.vzRunnerPath)
	if err != nil {
		return fmt.Errorf("vz-runner not found at %s: %w", v.vzRunnerPath, err)
	}

	if info.IsDir() {
		return fmt.Errorf("vz-runner is a directory, not an executable")
	}

	return nil
}

// SetQMPSocket sets the QMP socket path for this instance
func (v *VzDarwin) SetQMPSocket(path string) {
	v.qmpSocket = path
}
