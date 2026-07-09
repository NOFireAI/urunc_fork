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
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"

	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
)

// TODO: Find and set the correct size for the tmpfs in the host
const tmpfsSizeForNoRootfs = "65536k"

func tmpfsMount(target string, size string) specs.Mount {
	return specs.Mount{
		Type: "tmpfs", Source: "tmpfs", Destination: target,
		Options: []string{"nosuid", "noexec", "strictatime", "mode=1777", "size=" + size, "private"},
	}
}

func bindMount(source string, target string, private bool) specs.Mount {
	m := specs.Mount{Type: "bind", Source: source, Destination: target, Options: []string{"bind"}}
	if private {
		m.Options = append(m.Options, "private")
	}
	return m
}

func deviceFromHost(path string) (specs.LinuxDevice, error) {
	var devStat unix.Stat_t
	if err := unix.Stat(path, &devStat); err != nil {
		return specs.LinuxDevice{}, err
	}
	var devType string
	switch devStat.Mode & unix.S_IFMT {
	case unix.S_IFCHR:
		devType = "c"
	case unix.S_IFBLK:
		devType = "b"
	default:
		return specs.LinuxDevice{}, fmt.Errorf("%s is not a device node", path)
	}
	mode := os.FileMode(devStat.Mode & 0o777)
	uid, gid := devStat.Uid, devStat.Gid
	return specs.LinuxDevice{
		Path: path, Type: devType,
		Major: int64(unix.Major(uint64(devStat.Rdev))), Minor: int64(unix.Minor(uint64(devStat.Rdev))),
		FileMode: &mode, UID: &uid, GID: &gid,
	}, nil
}

type rootfsBuilder interface {
	preSetup() error
	postSetup() error
	getMounts() ([]specs.Mount, error)
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
	return nil
}

func (n noRootfs) getMounts() ([]specs.Mount, error) {
	return []specs.Mount{tmpfsMount("/tmp", tmpfsSizeForNoRootfs)}, nil
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

func getMonitorDevices(needsKVM bool) ([]specs.LinuxDevice, error) {
	nullDev, err := deviceFromHost("/dev/null")
	if err != nil {
		return nil, fmt.Errorf("could not get host device /dev/null: %w", err)
	}
	randomDev, err := deviceFromHost("/dev/urandom")
	if err != nil {
		return nil, fmt.Errorf("could not get host device /dev/urandom: %w", err)
	}
	devices := []specs.LinuxDevice{nullDev, randomDev}
	tunDev, err := deviceFromHost("/dev/net/tun")
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			uniklog.Warnf("could not find /dev/net/tun in host, skipping it")
		} else {
			return nil, fmt.Errorf("could not get host device /dev/net/tun: %w", err)
		}
	} else {
		devices = append(devices, tunDev)
	}
	if needsKVM {
		kvmDev, err := deviceFromHost("/dev/kvm")
		if err != nil {
			return nil, fmt.Errorf("could not get host device /dev/kvm: %w", err)
		}
		devices = append(devices, kvmDev)
	}
	return devices, nil
}

func setupConsole(monRootfs string) error {
	// Create /dev/ptmx as a symlink to /dev/pts/ptmx
	// This is the standard way to provide the PTY master device
	ptmxPath := filepath.Join(monRootfs, "/dev/ptmx")
	err := os.Symlink("pts/ptmx", ptmxPath)
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

func mountsForMonitor(monitorPath string, monitorDataPath string) ([]specs.Mount, error) {
	mounts := []specs.Mount{
		{Type: "proc", Source: "proc", Destination: "/proc"},
		{Type: "tmpfs", Source: "tmpfs", Destination: "/dev", Options: []string{"nosuid", "strictatime", "mode=755", "size=65536k", "private"}},
		{Type: "devpts", Source: "devpts", Destination: "/dev/pts", Options: []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620"}},
		bindMount(monitorPath, monitorPath, true),
	}
	monitorName := filepath.Base(monitorPath)
	if monitorName != "firecracker" {
		mounts = append(mounts, bindMount("/lib", "/lib", true))
		if _, err := os.Stat("/lib64"); err == nil {
			mounts = append(mounts, bindMount("/lib64", "/lib64", true))
		} else if !os.IsNotExist(err) {
			return nil, err
		}
		mounts = append(mounts, bindMount("/usr/lib", "/usr/lib", true))
	}
	if len(monitorName) >= 4 && monitorName[:4] == "qemu" {
		qDataPath := filepath.Join(monitorDataPath, "qemu")
		sBiosPath := filepath.Join(monitorDataPath, "seabios")
		if monitorDataPath == "" {
			var err error
			qDataPath, err = findQemuDataDir("qemu")
			if err != nil {
				return nil, err
			}
			sBiosPath, err = findQemuDataDir("seabios")
			if err != nil {
				return nil, fmt.Errorf("failed to get info of seabios directory: %w", err)
			}
		}
		mounts = append(mounts, bindMount(qDataPath, "/usr/share/qemu", true))
		if _, err := os.Stat(sBiosPath); err == nil {
			mounts = append(mounts, bindMount(sBiosPath, "/usr/share/seabios", true))
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	return mounts, nil
}
