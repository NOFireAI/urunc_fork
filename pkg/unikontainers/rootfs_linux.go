//go:build linux
// +build linux

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

// Linux-only rootfs realization: the mount-namespace operations (pivot_root,
// chroot, monitor rootfs preparation) and the noRootfs builder. The rootfs
// *selection* decision is platform-neutral and lives in rootfs.go.

package unikontainers

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
)

// TODO: Find and set the correct size for the tmpfs in the host
const tmpfsSizeForNoRootfs = "65536k"

type rootfsBuilder interface {
	preSetup() error
	postSetup() error
	getBlockDevs() ([]types.BlockDevParams, error)
	getSharedDirs() (types.SharedfsParams, error)
	preStart() error
}

type noRootfs struct {
	monRootfs            string
	annotBlockPath       string
	annotBlockMountPoint string
}

func (n noRootfs) preSetup() error {
	return nil
}

func (n noRootfs) postSetup() error {
	err := createTmpfs(n.monRootfs, "/tmp",
		unix.MS_NOSUID|unix.MS_NOEXEC|unix.MS_STRICTATIME,
		"1777", tmpfsSizeForNoRootfs)
	if err != nil {
		err = fmt.Errorf("failed to create tmpfs for monitor's execution environment: %w", err)
	}

	return err
}

func (n noRootfs) getBlockDevs() ([]types.BlockDevParams, error) {
	blkImgs := []types.BlockDevParams{}
	blockFromAnnot, err := handleExplicitBlockImage(n.annotBlockPath,
		n.annotBlockMountPoint)
	if err != nil {
		return nil, err
	}

	if blockFromAnnot.Source != "" && blockFromAnnot.MountPoint != "/" {
		// TODO: Add proper support for multiple block Images from the container's
		// image. This requires adding more annotations too.
		blockFromAnnot.ID = "annot_vol"
		blkImgs = append(blkImgs, blockFromAnnot)
	}

	return blkImgs, nil
}

// TODO: Return an array instead of a single struct
func (n noRootfs) getSharedDirs() (types.SharedfsParams, error) {
	return types.SharedfsParams{}, nil
}

func (n noRootfs) preStart() error {
	return nil
}

// pivotRootfs changes rootfs with pivot
// It should be called with CWD being the new rootfs
func pivotRootfs(newRoot string) error {
	// Set up directory of previous rootfs
	oldRoot := filepath.Join(newRoot, "/old_root")
	err := os.MkdirAll(oldRoot, 0755)
	if err != nil {
		return fmt.Errorf("failed to create directory %s: %w", oldRoot, err)
	}

	err = unix.PivotRoot(".", "old_root")
	if err != nil {
		return fmt.Errorf("failed to pivot root: %w", err)
	}

	// Make sure we are in the new rootfs
	err = os.Chdir("/")
	if err != nil {
		return fmt.Errorf("failed to set CWD as /: %w", err)
	}

	// Make oldroot rslave to make sure our unmounts don't propagate to the
	// host (and thus bork the machine). We don't use rprivate because this is
	// known to cause issues due to races where we still have a reference to a
	// mount while a process in the host namespace are trying to operate on
	// something they think has no mounts (devicemapper in particular).
	err = unix.Mount("", "old_root", "", unix.MS_SLAVE|unix.MS_REC, "")
	if err != nil {
		return fmt.Errorf("failed to make old_root rslave: %w", err)
	}

	// Perform the unmount. MNT_DETACH allows us to unmount /proc/self/cwd.
	err = unix.Unmount("old_root", unix.MNT_DETACH)
	if err != nil {
		return fmt.Errorf("failed to unmount old_root: %w", err)
	}

	// We no longer need the old rootfs
	err = os.RemoveAll("old_root")
	if err != nil {
		return fmt.Errorf("failed to remove old_root: %w", err)
	}

	return nil
}

// changeRoot changes the rootfs to rootfsDir. If pivot is true, then we will
// use pivot (requires mount namespaces), otherwise we will use chroot
func changeRoot(rootfsDir string, pivot bool) error {
	// Set CWD the rootfs of the container
	err := os.Chdir(rootfsDir)
	if err != nil {
		return err
	}

	if pivot {
		err = pivotRootfs(rootfsDir)
		if err != nil {
			return err
		}
	} else {
		err = unix.Chroot(".")
		if err != nil {
			return err
		}
	}

	// Set CWD the rootfs of the container to ensure we are in the new rootfs
	err = os.Chdir("/")
	if err != nil {
		return err
	}

	return nil
}

// nolint:gocyclo
// prepareMonRootfs prepares the rootfs where the monitor will execute. It
// essentially sets up the devices (KVM, snapshotter block device) that are required
// for the guest execution and any other files (e.g. binaries).
func prepareMonRootfs(monRootfs string, monitorPath string, monitorDataPath string, needsKVM bool, needsTAP bool) error {
	err := fileFromHost(monRootfs, monitorPath, "", unix.MS_BIND|unix.MS_PRIVATE, false)
	if err != nil {
		return err
	}

	// TODO: Remove these when we switch to static binaries
	monitorName := filepath.Base(monitorPath)
	if monitorName != "firecracker" {
		err = fileFromHost(monRootfs, "/lib", "", unix.MS_BIND|unix.MS_PRIVATE, false)
		if err != nil {
			return err
		}

		err = fileFromHost(monRootfs, "/lib64", "", unix.MS_BIND|unix.MS_PRIVATE, false)
		if err != nil {
			// If the file does not exist, just ignore it
			if !os.IsNotExist(err) {
				return err
			}
		}

		err = fileFromHost(monRootfs, "/usr/lib", "", unix.MS_BIND|unix.MS_PRIVATE, false)
		if err != nil {
			return err
		}
	}

	// TODO: Remove these when we switch to static binaries
	if len(monitorName) >= 4 && monitorName[:4] == "qemu" {
		var qDataPath string
		var sBiosPath string
		var err error
		if monitorDataPath == "" {
			qDataPath, err = findQemuDataDir("qemu")
		} else {
			qDataPath = filepath.Join(monitorDataPath, "qemu")
			err = nil
		}
		if err != nil {
			return err
		}

		err = fileFromHost(monRootfs, qDataPath, "/usr/share/qemu", unix.MS_BIND|unix.MS_PRIVATE, false)
		if err != nil {
			return err
		}

		if monitorDataPath == "" {
			sBiosPath, err = findQemuDataDir("seabios")
		} else {
			sBiosPath = filepath.Join(monitorDataPath, "seabios")
			err = nil
		}
		if err != nil {
			return fmt.Errorf("failed to get info of seabios directory: %w", err)
		}
		err = fileFromHost(monRootfs, sBiosPath, "/usr/share/seabios", unix.MS_BIND|unix.MS_PRIVATE, false)
		if err != nil {
			// In urunc-deploy and in some distros seabios does not exist and
			// we do not need it. So if we could not find it, just ignore it.
			if !os.IsNotExist(err) {
				return err
			}
		}
	}

	newProcDir := filepath.Join(monRootfs, "/proc")
	err = os.MkdirAll(newProcDir, 0555)
	if err != nil {
		return err
	}

	err = unix.Mount("proc", newProcDir, "proc", 0, "")
	if err != nil {
		return err
	}

	err = createTmpfs(monRootfs, "/dev", unix.MS_NOSUID|unix.MS_STRICTATIME, "755", "65536k")
	if err != nil {
		return err
	}

	err = setupDev(monRootfs, "/dev/null")
	if err != nil {
		return err
	}

	err = setupDev(monRootfs, "/dev/urandom")
	if err != nil {
		return err
	}

	if needsTAP {
		err = setupDev(monRootfs, "/dev/net/tun")
		if err != nil {
			return err
		}
	}

	if needsKVM {
		err = setupDev(monRootfs, "/dev/kvm")
		if err != nil {
			return err
		}
	}

	// Setup /dev/pts for PTY support (needed for console and debugging tools like cntr)
	// This allows tools like cntr to attach to the container with a shell
	devPtsDir := filepath.Join(monRootfs, "/dev/pts")
	err = os.MkdirAll(devPtsDir, 0755)
	if err != nil {
		return fmt.Errorf("failed to create /dev/pts directory: %w", err)
	}

	// Mount devpts filesystem
	// Using newinstance creates an isolated pts namespace for this container
	err = unix.Mount("devpts", devPtsDir, "devpts", unix.MS_NOSUID|unix.MS_NOEXEC, "newinstance,ptmxmode=0666,mode=0620,gid=5")
	if err != nil {
		return fmt.Errorf("failed to mount devpts: %w", err)
	}

	// Create /dev/ptmx as a symlink to /dev/pts/ptmx
	// This is the standard way to provide the PTY master device
	ptmxPath := filepath.Join(monRootfs, "/dev/ptmx")
	err = os.Symlink("pts/ptmx", ptmxPath)
	if err != nil && !os.IsExist(err) {
		return fmt.Errorf("failed to create /dev/ptmx symlink: %w", err)
	}

	// Create /dev/console file
	consolePath := filepath.Join(monRootfs, "/dev/console")
	consoleFile, err := os.OpenFile(consolePath, os.O_CREATE|os.O_WRONLY, 0o666)
	if err != nil {
		return fmt.Errorf("failed to create /dev/console: %w", err)
	}
	defer consoleFile.Close()

	// Ensure correct permissions
	if err := consoleFile.Chmod(0o666); err != nil {
		return fmt.Errorf("failed to chmod /dev/console: %w", err)
	}

	return nil
}
