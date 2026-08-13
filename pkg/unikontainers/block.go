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

package unikontainers

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/moby/sys/mountinfo"
	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/sirupsen/logrus"
	"github.com/urunc-dev/urunc/pkg/unikontainers/initrd"
	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
	"golang.org/x/sys/unix"
)

// TODO: Find and set the correct size for the tmpfs in the host
const tmpfsSizeForBlockRootfs = "65536k"

// vmiBootInitrdGuestPath is the guest-relative path where the Option-C
// telemetry boot initrd is staged for the block variant. The customer rootfs is
// delivered as a separate block device (/dev/vda); this initrd is handed to the
// VMM as the boot initrd (rdinit=/init), and its /init switch_root's into the
// customer rootfs after starting telemetry. Kept distinct from the
// com.urunc.unikernel.initrd annotation, which would otherwise make the initrd
// itself the guest rootfs (ChooseRootfs.tryInitrd).
const vmiBootInitrdGuestPath = "/boot/urunc-vmi-initrd"

// vmiCmdGuestPath is the in-initrd file where the customer entrypoint argv is
// baked (one argument per line). vmi-init reads it instead of relying on the
// kernel cmdline, whose tokenizer word-splits quoted multi-word arguments.
const vmiCmdGuestPath = "/vmi-cmd"

// vmiEnvGuestPath is the in-initrd file holding the container environment (one
// KEY=VALUE per line). It exists because the alternative - appending the
// environment to the kernel cmdline - publishes every value on the guest
// console, in /proc/cmdline to every process in the guest, and into the
// captured telemetry. That is fatal for secrets such as API tokens.
const vmiEnvGuestPath = "/vmi-env"

// vmiResolvGuestPath is the in-initrd file holding the resolver directives
// carried over from the host; vmi-init installs it into the customer rootfs
// when that rootfs has no usable /etc/resolv.conf of its own.
const vmiResolvGuestPath = "/vmi-resolv.conf"

// vmiHostsGuestPath and vmiHostnameGuestPath are the in-initrd staging paths
// for the container's /etc/hosts and /etc/hostname.
const vmiHostsGuestPath = "/vmi-hosts"
const vmiHostnameGuestPath = "/vmi-hostname"

var ErrMountpoint = errors.New("no FS is mounted in this mountpoint")

type blockRootfs struct {
	mounts        []specs.Mount
	monRootfs     string
	mountedPath   string
	path          string
	kernelPath    string
	initrdPath    string
	uruncJSONPath string
	guestType     string
	guest         types.Unikernel
	// VMI (Option C) host-supplied boot files. When vmiIntrospect is true the
	// kernel and telemetry initrd are staged from the host into the monitor
	// rootfs instead of extracted from the (unmodified) customer image.
	vmiIntrospect bool
	vmiKernelHost string
	vmiInitrdHost string
	vmiPayloadDir string
	// vmiCmd is the customer entrypoint argv (OCI process.args), baked into the
	// boot initrd as /vmi-cmd so vmi-init can exec it verbatim after switch_root.
	vmiCmd []string
	// vmiEnv is the container environment (OCI process.env), baked into the boot
	// initrd as /vmi-env so it never has to travel on the kernel cmdline.
	vmiEnv []string
}

// getMountInfo determines whether the provided path is a mount point
// by inspecting /proc/self/mountinfo.
// If the path is a mount point, it populates and returns a BlockDevParams struct.
// Otherwise, it returns an error along with an empty BlockDevParams.
// Additionally, when the path is a mount point, getMountInfo verifies
// the mount source to ensure it can use the source as a block device.
// There are cases (e.g. bind mounts) where mounts use the same underlying
// source device as the original mount, so they can appear identical to
// regular mounts when inspecting mount information.
func getMountInfo(path string) (types.BlockDevParams, error) {
	selfProcMountInfo := "/proc/self/mountinfo"

	file, err := os.Open(selfProcMountInfo)
	if err != nil {
		return types.BlockDevParams{}, fmt.Errorf("failed to open mountinfo: %w", err)
	}
	defer file.Close()

	blockDev := types.BlockDevParams{}
	nonSpecialSources := make(map[string]struct{})
	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		line := scanner.Text()
		parts := strings.Split(line, " - ")
		if len(parts) != 2 {
			return types.BlockDevParams{}, fmt.Errorf("invalid mountinfo line in /proc/self/mountinfo")
		}

		preDash := strings.Fields(parts[0])
		if len(preDash) < 6 {
			continue
		}
		postDash := strings.Fields(parts[1])
		if len(postDash) < 2 {
			continue
		}
		if preDash[4] == path {
			uniklog.WithFields(logrus.Fields{
				"mounted at": path,
				"device":     postDash[1],
				"fstype":     postDash[0],
				"options":    preDash[5],
			}).Debug("Found block device")

			blockDev.Source = postDash[1]
			blockDev.FsType = postDash[0]
			blockDev.MountPoint = path
			// Keep the mount VFS options (field 6 of mountinfo)
			// to restore them later in the delete path.
			blockDev.MountOptions = preDash[5]
			blockDev.ID = ""
			continue
		}
		// Store the source of all mounts with non-special fs
		// (e.g. overlay, tmpfs) in a map
		if postDash[0] != postDash[1] {
			nonSpecialSources[postDash[1]] = struct{}{}
		}
	}

	if blockDev.Source == "" {
		return types.BlockDevParams{}, ErrMountpoint
	}

	// Check if the source of the mountpoint that refers to path
	// exists i the map with the found sources. If this is the case,
	// then we are not dealing with a mount regarding a block device
	// that we can attach to the sandbox.
	_, ok := nonSpecialSources[blockDev.Source]
	if ok {
		return types.BlockDevParams{}, ErrMountpoint
	}

	return blockDev, nil
}

// extractUnikernelFromBlock moves unikernel binary, initrd and urunc.json
// files from old rootfsPath to newRootfsPath
// FIXME: This approach fills up /run with unikernel binaries, initrds and urunc.json
// files for each unikernel we run
func extractBootFiles(rootfsPath string, newRootfsPath string, unikernel string, uruncJSON string, initrd string) error {
	currentUnikernelPath := filepath.Join(rootfsPath, unikernel)
	targetUnikernelPath := filepath.Join(newRootfsPath, unikernel)
	targetUnikernelDir, _ := filepath.Split(targetUnikernelPath)
	err := moveFile(currentUnikernelPath, targetUnikernelDir)
	if err != nil {
		return fmt.Errorf("could not move %s to %s: %w", currentUnikernelPath, targetUnikernelPath, err)
	}

	if initrd != "" {
		currentInitrdPath := filepath.Join(rootfsPath, initrd)
		targetInitrdPath := filepath.Join(newRootfsPath, initrd)
		targetInitrdDir, _ := filepath.Split(targetInitrdPath)
		err = moveFile(currentInitrdPath, targetInitrdDir)
		if err != nil {
			return fmt.Errorf("could not move %s to %s: %w", currentInitrdPath, targetInitrdPath, err)
		}
	}

	currentConfigPath := filepath.Join(rootfsPath, uruncJSON)
	err = moveFile(currentConfigPath, newRootfsPath)
	if err != nil {
		return fmt.Errorf("could not move %s to %s: %w", currentConfigPath, newRootfsPath, err)
	}

	return nil
}

// stageVMIBootFiles implements Option C boot-file provisioning: it copies the
// host kernel and the host-built telemetry initrd into the monitor rootfs at the
// guest-relative boot paths (b.kernelPath / b.initrdPath), then augments the
// per-container initrd copy with the VMI telemetry payload. The customer image
// is never read for boot files, so an unmodified OCI image boots with full
// telemetry. The customer filesystem itself is delivered separately as the guest
// block device (/dev/vda) and pivoted into by the injected /init.
func (b blockRootfs) stageVMIBootFiles() error {
	if b.vmiKernelHost == "" {
		return fmt.Errorf("VMI introspection requires a host kernel via %s", AnnotVMIKernel)
	}
	if b.kernelPath == "" {
		return fmt.Errorf("VMI introspection requires a guest kernel path (%s)", annotBinary)
	}
	if b.vmiInitrdHost == "" {
		return fmt.Errorf("VMI introspection requires a host boot initrd via %s", AnnotVMIInitrd)
	}

	kernelDst := filepath.Join(b.monRootfs, b.kernelPath)
	if err := copyFile(b.vmiKernelHost, kernelDst); err != nil {
		return fmt.Errorf("could not stage host kernel %s: %w", b.vmiKernelHost, err)
	}

	// The customer rootfs is a separate block device, so the telemetry initrd is
	// staged at a fixed guest path and passed to the VMM as the boot initrd. The
	// START phase resolves the same path for vmmArgs.InitrdPath.
	initrdDst := filepath.Join(b.monRootfs, vmiBootInitrdGuestPath)
	if err := copyFile(b.vmiInitrdHost, initrdDst); err != nil {
		return fmt.Errorf("could not stage host initrd %s: %w", b.vmiInitrdHost, err)
	}

	// Layer the telemetry payload (init wrapper + busybox + telem-capture +
	// urunit-agent) onto the per-container initrd copy so capture binaries can
	// be refreshed without rebuilding the base initrd.
	if err := initrd.AugmentInitrdForVMI(initrdDst, b.vmiPayloadDir); err != nil {
		return fmt.Errorf("could not augment initrd with VMI payload: %w", err)
	}

	// Bake the customer entrypoint argv into the initrd (one arg per line) so
	// vmi-init execs it verbatim after switch_root, bypassing the kernel cmdline
	// tokenizer which word-splits quoted multi-word arguments.
	if len(b.vmiCmd) > 0 {
		if err := initrd.AddFileToInitrd(initrdDst, strings.Join(b.vmiCmd, "\n")+"\n", vmiCmdGuestPath); err != nil {
			return fmt.Errorf("could not write VMI command file to initrd: %w", err)
		}
	}

	// Keep the environment off the kernel cmdline: vmi-init exports it from here.
	if len(b.vmiEnv) > 0 {
		if err := initrd.AddFileToInitrd(initrdDst, strings.Join(b.vmiEnv, "\n")+"\n", vmiEnvGuestPath); err != nil {
			return fmt.Errorf("could not write VMI env file to initrd: %w", err)
		}
	}

	// The runtime is handed /etc/resolv.conf, /etc/hosts and /etc/hostname as
	// bind mounts carrying the container's network identity. A normal runtime
	// applies them; booting the image as a block device ignores them, so the
	// guest comes up with an empty resolv.conf - raw IP egress works and every
	// name lookup fails. Carry them in through the initrd instead.
	if err := b.stageVMINetworkFiles(initrdDst); err != nil {
		return err
	}

	// Stage a custom firecracker CPU template into the monitor rootfs so the
	// pivot_root'd VMM can read it at "/cpu-template.json" (kept in sync with
	// hypervisors.stagedCPUTemplatePath). Optional: off unless the env var is
	// set, for hosts that genuinely need the guest CPUID normalised.
	if src := os.Getenv("URUNC_FC_CPU_CONFIG"); src != "" {
		if err := copyFile(src, filepath.Join(b.monRootfs, "cpu-template.json")); err != nil {
			return fmt.Errorf("could not stage cpu template %s: %w", src, err)
		}
	}

	return nil
}

// vmiNetworkFiles maps an OCI mount destination to the in-initrd path where its
// contents are staged for vmi-init to install into the customer rootfs.
var vmiNetworkFiles = map[string]string{
	"/etc/resolv.conf": vmiResolvGuestPath,
	"/etc/hosts":       vmiHostsGuestPath,
	"/etc/hostname":    vmiHostnameGuestPath,
}

// stageVMINetworkFiles copies the container's network identity files out of the
// OCI bind mounts and into the boot initrd. If the runtime supplied no
// resolv.conf mount, the host's resolver is used as a fallback so the guest is
// not left unable to resolve anything at all.
func (b blockRootfs) stageVMINetworkFiles(initrdDst string) error {
	staged := make(map[string]bool, len(vmiNetworkFiles))
	for _, m := range b.mounts {
		dest, ok := vmiNetworkFiles[m.Destination]
		if !ok || m.Source == "" {
			continue
		}
		data, err := os.ReadFile(m.Source)
		if err != nil {
			uniklog.WithError(err).WithField("source", m.Source).
				Warn("could not read container network file; guest will not get it")
			continue
		}
		if len(data) == 0 {
			continue
		}
		if err := initrd.AddFileToInitrd(initrdDst, string(data), dest); err != nil {
			return fmt.Errorf("could not write %s to initrd: %w", dest, err)
		}
		staged[m.Destination] = true
	}
	if !staged["/etc/resolv.conf"] {
		if resolv := hostResolvConf(); resolv != "" {
			if err := initrd.AddFileToInitrd(initrdDst, resolv, vmiResolvGuestPath); err != nil {
				return fmt.Errorf("could not write VMI resolv.conf to initrd: %w", err)
			}
		}
	}
	return nil
}

// hostResolvConf returns resolver directives to carry into the guest, or "" if
// the host has none worth carrying. systemd-resolved's /etc/resolv.conf is a
// stub pointing at a loopback listener (127.0.0.53) that does not exist in the
// guest, so prefer its recorded upstreams and drop loopback nameservers - the
// same substitution docker makes. Nothing is invented if that leaves nothing:
// an absent resolv.conf is easier to diagnose than one pointing somewhere the
// operator never chose.
func hostResolvConf() string {
	for _, src := range []string{"/run/systemd/resolve/resolv.conf", "/etc/resolv.conf"} {
		data, err := os.ReadFile(src)
		if err != nil {
			continue
		}
		var out []string
		var haveNS bool
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			switch fields[0] {
			case "nameserver":
				if ip := net.ParseIP(fields[1]); ip == nil || ip.IsLoopback() {
					continue
				}
				haveNS = true
				out = append(out, line)
			case "search", "options":
				out = append(out, line)
			}
		}
		if haveNS {
			return strings.Join(out, "\n") + "\n"
		}
	}
	return ""
}

func copyMountfiles(targetPath string, mounts []specs.Mount) error {
	for _, m := range mounts {
		if m.Type != "bind" {
			continue
		}
		err := fileFromHost(targetPath, m.Source, m.Destination)
		if (err != nil) && !errors.Is(err, ErrCopyDir) {
			return err
		}
	}

	return nil
}

func handleExplicitBlockImage(blockImg string, mountPoint string) (types.BlockDevParams, error) {
	if blockImg == "" {
		return types.BlockDevParams{}, nil
	}

	if mountPoint == "" {
		return types.BlockDevParams{}, fmt.Errorf("annotation for block device was set without a mountpoint")
	}

	id := ""
	if mountPoint == "/" {
		id = "rootfs"
	}

	return types.BlockDevParams{
		Source:     blockImg,
		MountPoint: mountPoint,
		ID:         id,
	}, nil
}

// Search all the mount entries in the container's config and
// find the ones that come from a block.
func getBlockVolumes(mounts []specs.Mount, ukernel types.Unikernel) ([]types.BlockDevParams, error) {
	blkImgs := []types.BlockDevParams{}
	for i, m := range mounts {
		// We check only bind mounts
		if m.Type != "bind" {
			continue
		}
		// Get the information of the source path
		// from /proc/self/mountinfo
		mInfo, err := getMountInfo(m.Source)
		if errors.Is(err, ErrMountpoint) {
			// ErrMountpoint means we did not find any
			// such mount and hence we can skip it.
			continue
		}
		if err != nil {
			return nil, err
		}
		if ukernel.SupportsFS(mInfo.FsType) {
			// So, there was an issue which was manifested from the testing.
			// If we have a file (e.g. ext2) and mount it, then we use
			// a loop device for the mount and this is what is shown
			// in the mount list. However, since we perform the unmount
			// the device might also get removed. See
			// https://www.kernel.org/doc/Documentation/ABI/testing/sysfs-block-loop
			// If the device gets removed, then we attach nothing to the
			// sandbox. To resolve this we remove the autoclear flag
			// and therefore the device will persist.
			// NOTE: Although we restore the autoclear flag in the delete path,
			// if delete is never called then the autoclear flag will never
			// get restored.and remounted
			// TODO: Add the above note in a documentation for storage
			// handling
			cleared, err := setLoopAutoclear(mInfo.Source, false)
			if err != nil {
				return nil, err
			}
			mInfo.LoopAutoclear = cleared
			err = unmount(mInfo.MountPoint)
			if err != nil {
				return nil, err
			}
			mInfo.ID = fmt.Sprintf("vol%d", i)
			mInfo.HostMountPoint = mInfo.MountPoint
			mInfo.MountPoint = m.Destination
			blkImgs = append(blkImgs, mInfo)
		}
	}

	return blkImgs, nil
}

// restoreBlockVolumes mounts the block volume sources that were unmounted
// during create
func restoreBlockVolumes(blockArgs []types.BlockDevParams) error {
	for _, b := range blockArgs {
		// Only volumes gathered from the container's mounts carry the
		// host mountpoint where their source was originally mounted.
		if b.HostMountPoint == "" {
			continue
		}
		mounted, err := mountinfo.Mounted(b.HostMountPoint)
		if err != nil {
			return fmt.Errorf("failed to check if %s is a mountpoint: %w", b.HostMountPoint, err)
		}
		if mounted {
			continue
		}

		// restoring block volumes is a simple mount operation, but using
		// containerd can lead to errors because the mount options parser of
		// containerd might not handle some VFS flags correctly and misplace
		// them in the options argument of mount system call.
		// Therefore, do not use containerd for such mounts and handle them
		// directly.
		var flags uintptr
		for _, o := range strings.Split(b.MountOptions, ",") {
			flag, clearFlag, err := mapVFSFlag(o)
			if err != nil {
				continue
			}
			if clearFlag {
				flags &^= flag
			} else {
				flags |= flag
			}
		}
		err = unix.Mount(b.Source, b.HostMountPoint, b.FsType, flags, "")
		if err != nil {
			return fmt.Errorf("failed to remount %s at %s: %w", b.Source, b.HostMountPoint, err)
		}

		if b.LoopAutoclear {
			_, err = setLoopAutoclear(b.Source, true)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

// setLoopAutoclear sets or clears the autoclear flag of a loop device and
// returns true when the flag was changed. It returns false without an error
// when the path is not a loop device or the flag already has the wanted value.
func setLoopAutoclear(devPath string, autoclear bool) (bool, error) {
	_, err := os.Stat(filepath.Join("/sys/class/block", filepath.Base(devPath), "loop"))
	if os.IsNotExist(err) {
		// Not a loop device
		return false, nil
	}
	if err != nil {
		return false, err
	}

	dev, err := os.OpenFile(devPath, os.O_RDWR, 0)
	if err != nil {
		return false, fmt.Errorf("failed to open %s: %w", devPath, err)
	}
	defer dev.Close()

	info, err := unix.IoctlLoopGetStatus64(int(dev.Fd()))
	if err != nil {
		return false, fmt.Errorf("failed to get the status of %s: %w", devPath, err)
	}
	if autoclear == (info.Flags&unix.LO_FLAGS_AUTOCLEAR != 0) {
		return false, nil
	}

	if autoclear {
		info.Flags |= unix.LO_FLAGS_AUTOCLEAR
	} else {
		info.Flags &^= unix.LO_FLAGS_AUTOCLEAR
	}
	err = unix.IoctlLoopSetStatus64(int(dev.Fd()), info)
	if err != nil {
		return false, fmt.Errorf("failed to set the status of %s: %w", devPath, err)
	}

	return true, nil
}

// blockDevNodes transforms a list of types.BlockDevParams to a list of
// specs.LinuxDevice which cna be then used for replicating these block
// devices form the host to the monitor's execution environment.
func blockDevNodes(blockArgs []types.BlockDevParams, rootfs types.RootfsParams) ([]specs.LinuxDevice, error) {
	if rootfs.Type != "block" {
		return nil, nil
	}

	var blockDevs []specs.LinuxDevice
	for _, b := range blockArgs {
		// The rootfs device is a real host device only when a container rootfs
		// was converted to a block device (MountedPath set). When it is an
		// explicit block image referenced by an annotation, no node is created.
		if b.Source == rootfs.Path && rootfs.MountedPath == "" {
			continue
		}
		bDev, err := deviceFromHost(b.Source)
		if err != nil {
			return nil, err
		}
		blockDevs = append(blockDevs, bDev)
	}

	return blockDevs, nil
}

func (b blockRootfs) preSetup() error {
	// Option C stages boot files from the host, so it must run even when the
	// container image is not mounted (mountedPath == ""), which is the case for
	// a devmapper block rootfs. The non-VMI path still requires the mounted
	// image to extract boot files from it.
	if b.mountedPath == "" && !b.vmiIntrospect {
		return nil
	}

	if b.mountedPath != "" {
		if err := copyMountfiles(b.mountedPath, b.mounts); err != nil {
			return fmt.Errorf("failed to copy files from mount list: %w", err)
		}
	}

	// Option C: for an unmodified customer image the kernel + telemetry initrd
	// come from the host, not the image. Stage them into the monitor rootfs so
	// the pivot_root'd VMM finds them at the guest-relative boot paths.
	if b.vmiIntrospect {
		if err := b.stageVMIBootFiles(); err != nil {
			return fmt.Errorf("failed to stage host VMI boot files: %w", err)
		}
	} else {
		// FIXME: This approach fills up /run with unikernel binaries and
		// urunc.json files for each unikernel instance we run
		err := extractBootFiles(b.mountedPath, b.monRootfs, b.kernelPath, b.uruncJSONPath, b.initrdPath)
		if err != nil {
			return fmt.Errorf("failed to extract boot files from rootfs: %w", err)
		}
	}

	if b.mountedPath != "" {
		if err := unmount(b.mountedPath); err != nil {
			return fmt.Errorf("failed to unmount rootfs: %w", err)
		}
	}

	return nil
}

func (b blockRootfs) postSetup() error {
	return nil
}

func (b blockRootfs) getMounts() ([]specs.Mount, error) {
	return []specs.Mount{tmpfsMount("/tmp", tmpfsSizeForBlockRootfs)}, nil
}

func (b blockRootfs) getBlockDevs() ([]types.BlockDevParams, error) {
	var blockArgs []types.BlockDevParams
	rootfsBlock := types.BlockDevParams{
		Source:     b.path,
		MountPoint: "/",
		ID:         "rootfs",
	}

	// NOTE: Rumprun does not allow us to mount
	// anything at '/'. As a result, we use the
	// /data mount point for Rumprun. For all the
	// other guests we use '/'.
	if b.guestType == "rumprun" {
		rootfsBlock.MountPoint = "/data"
	}

	blockArgs = append(blockArgs, rootfsBlock)
	blockFromMounts, err := getBlockVolumes(b.mounts, b.guest)
	if err != nil {
		return nil, err
	}
	blockArgs = append(blockArgs, blockFromMounts...)

	return blockArgs, nil
}

// TODO: Return an array instead of a single struct
func (b blockRootfs) getSharedDirs() (types.SharedfsParams, error) {
	return types.SharedfsParams{}, nil
}

func (b blockRootfs) preStart() error {
	return nil
}

// Taken from https://github.com/containerd/containerd/blob/v1.7.34/mount/mount_linux.go#L203
// and we simply change the timeout period to max 200 ms, then EBUSY is returned.
func unmount(target string) error {
	for i := 0; i < 10; i++ {
		// Always aim for strict unmount
		err := unix.Unmount(target, 0)
		if err != nil {
			switch err {
			case unix.EBUSY:
				time.Sleep(20 * time.Millisecond)
				continue
			default:
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("failed to unmount target %s: %w", target, unix.EBUSY)
}
