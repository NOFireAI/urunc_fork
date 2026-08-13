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

// Runtime-spawned VMI introspection: when a container opts in via annotations,
// the shim brings up the vmi `sidecar` alongside the guest VMM and reaps it with
// the container. This replaces the external introspector / manual nsenter+attach
// that had to discover the VMM pid and the control socket after the fact.
//
// The sidecar attaches to the forked VMM through the control socket the VMM
// itself creates (/urunc-telem.sock, visible as /proc/<pid>/root/urunc-telem.sock)
// and observes the guest tap inside the container network namespace. All of this
// is knowable from the task init pid the shim already holds, so no discovery or
// timing race is needed.
//
// Everything here is BEST-EFFORT: any failure (no control socket, no kernel, no
// sidecar binary, spawn error) is logged and the guest keeps running.

package unikontainers

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"
)

const (
	// AnnotVMIIntrospect gates the whole feature; anything but "true" is off.
	// AnnotVMIKernel is the guest vmlinux (with BTF) the sidecar resolves kernel
	// struct offsets from. Required: with the control socket, the kernel view
	// needs it, so without it we skip rather than run a half-configured sidecar.
	// AnnotVMIBootKernel is the image the monitor boots, when that has to differ
	// from the vmlinux the sidecar reads offsets out of. Optional: it defaults to
	// AnnotVMIKernel, which is right whenever one file serves both, as it does
	// for Firecracker.
	//
	// hvi is why this exists. Its x86 loader takes a bzImage and rejects an ELF
	// vmlinux ("bad bzImage boot flag"), while the sidecar's offset resolver
	// needs the ELF and rejects the bzImage ("missing 64-bit little-endian ELF").
	// One annotation cannot satisfy both, and pointing it at either one alone
	// costs the other: the guest will not boot, or it boots and emits nothing.
	// AnnotVMIIntrospectInterval makes the monitor introspect on its own, every
	// N seconds, in addition to serving any out-of-VMM reader. It is what the
	// in-VMM vantage needs to have anything to measure: without it hvi only
	// walks when someone asks over the control socket, which is the out-of-VMM
	// path and therefore the wrong thing to time.
	// AnnotVMIWalkBench repeats the monitor own walk N times per introspection
	// and reports the mean. One walk is a cold walk, and comparing that against
	// an out-of-VMM reader running many warm ones measures the warm-up rather
	// than the vantage. Needed for a matched comparison, not for the feature.
	// AnnotVMIEventsOut is a path (inside the monitor mount namespace) for
	// the VMM's own events ledger; hvi takes it as --events.
	// AnnotVMIGPA is the guest-physical base of the telemetry region (hex or
	// decimal); defaults to the sidecar's own default when unset.
	// AnnotVMIEmit selects the sidecar emit mode (e.g. "both").
	// AnnotVMISchema selects the sidecar schema (e.g. "both").
	// AnnotVMIOutput is the NDJSON sink path; defaults to a per-container file
	// under defaultVMIOutputDir.
	// AnnotVMIDisk names the guest's backing block device for the sidecar's
	// ext4-journal filesystem source. Normally left unset: the shim resolves the
	// devmapper node from the VMM's open fds. Set it when the rootfs is backed by
	// something the fd scan will not recognise.
	// AnnotVMISidecarBin overrides the sidecar binary path.
	// AnnotVMITap overrides the guest tap the sidecar observes.
	// AnnotVMIPayloadDir overrides the host dir holding the initrd telemetry
	// payload (Option C zero-image-change injection); defaults to
	// defaultVMIPayloadDir. Staged by packaging/vmi-initrd/build-vmi-payload.sh.
	// AnnotVMIInitrd is the HOST path to the injectable telemetry initrd built
	// by packaging/vmi-initrd/build-vmi-payload.sh. For a stock image (no baked
	// initrd) urunc stages this into the monitor rootfs instead of extracting an
	// initrd from the image. Empty => fall back to per-container augmentation.

	defaultVMISidecarBin = "/usr/local/bin/sidecar"
	defaultVMITap        = "tap0_urunc"
	defaultVMIGPA        = "0x40000000"
	defaultVMIEmit       = "both"
	defaultVMISchema     = "both"
	defaultVMIOutputDir  = "/run/urunc/vmi"
	defaultVMIPayloadDir = "/opt/urunc-vmi/payload"

	// controlSockName is the file the forked VMM binds inside its own mount ns.
	controlSockName = "urunc-telem.sock"
	// controlSockWait bounds how long we wait for the VMM to create the control
	// socket after start before giving up (best-effort).
	controlSockWait = 15 * time.Second
	// sidecarStopGrace is how long we let the sidecar exit on SIGTERM before
	// escalating to SIGKILL on teardown.
	sidecarStopGrace = 3 * time.Second
)

// VMIConfig is the parsed, per-container introspection configuration.
type VMIConfig struct {
	Introspect bool
	Kernel     string
	BootKernel string
	GPA        string
	Emit       string
	Schema     string
	Output     string
	SidecarBin string
	Tap        string
	PayloadDir string
	Initrd     string
	Disk       string
}

// BootKernelPath is the image to stage as the guest's boot kernel: the explicit
// boot kernel when the image needs a different format from the one the sidecar
// reads, and otherwise the sidecar's kernel.
func (c VMIConfig) BootKernelPath() string {
	if c.BootKernel != "" {
		return c.BootKernel
	}
	return c.Kernel
}

// VMIConfigFromAnnotations parses a VMIConfig from OCI annotations, filling
// defaults. Introspect is true only when the gate annotation is exactly "true".
func VMIConfigFromAnnotations(a map[string]string) VMIConfig {
	get := func(k, def string) string {
		if v, ok := a[k]; ok && v != "" {
			return v
		}
		return def
	}
	return VMIConfig{
		Introspect: a[AnnotVMIIntrospect] == "true",
		Kernel:     a[AnnotVMIKernel],
		BootKernel: a[AnnotVMIBootKernel],
		GPA:        get(AnnotVMIGPA, defaultVMIGPA),
		Emit:       get(AnnotVMIEmit, defaultVMIEmit),
		Schema:     get(AnnotVMISchema, defaultVMISchema),
		Disk:       a[AnnotVMIDisk],
		Output:     a[AnnotVMIOutput], // resolved per-container in Spawn when empty
		SidecarBin: get(AnnotVMISidecarBin, defaultVMISidecarBin),
		Tap:        get(AnnotVMITap, defaultVMITap),
		PayloadDir: get(AnnotVMIPayloadDir, defaultVMIPayloadDir),
		Initrd:     a[AnnotVMIInitrd],
	}
}

// bundleAnnotations reads the OCI runtime spec (config.json) from a bundle and
// returns its annotations. Used by the shim, which holds the bundle path at
// Create.
func bundleAnnotations(bundle string) (map[string]string, error) {
	data, err := os.ReadFile(filepath.Join(bundle, "config.json"))
	if err != nil {
		return nil, err
	}
	var spec struct {
		Annotations map[string]string `json:"annotations"`
	}
	if err := json.Unmarshal(data, &spec); err != nil {
		return nil, err
	}
	return spec.Annotations, nil
}

// VMIConfigFromBundle parses the introspection config from a bundle's spec.
func VMIConfigFromBundle(bundle string) (VMIConfig, error) {
	a, err := bundleAnnotations(bundle)
	if err != nil {
		return VMIConfig{}, err
	}
	return VMIConfigFromAnnotations(a), nil
}

// VMISidecar is a running, supervised sidecar bound to one container's VMM.
type VMISidecar struct {
	id   string
	cmd  *exec.Cmd
	done chan struct{}
}

// resolveVMIDisk finds the guest's backing block device by reading the VMM's open
// fds, the same way the external introspector did. Returns "" when nothing looks
// like a block device, in which case the sidecar simply runs without a
// filesystem plane rather than failing to start.
func resolveVMIDisk(vmmPid int) string {
	fdDir := filepath.Join("/proc", strconv.Itoa(vmmPid), "fd")
	entries, err := os.ReadDir(fdDir)
	if err != nil {
		return ""
	}
	fallback := ""
	for _, e := range entries {
		target, err := os.Readlink(filepath.Join(fdDir, e.Name()))
		if err != nil {
			continue
		}
		// Prefer the named mapper node; /dev/dm-N is the same device by a less
		// stable name, so keep it only as a fallback.
		if strings.HasPrefix(target, "/dev/mapper/") {
			return target
		}
		if fallback == "" && strings.HasPrefix(target, "/dev/dm-") {
			fallback = target
		}
	}
	return fallback
}

// SpawnVMISidecar launches the sidecar for a started container whose VMM runs as
// vmmPid, wired to that VMM's control socket and running inside the VMM's network
// namespace so it can observe the guest tap.
//
// It is BEST-EFFORT: it returns an error only so the caller can log it; the
// caller must never fail the task on that error.
//
// The sidecar is entered into the container netns via `nsenter -t <pid> -n`,
// which mirrors the manual attach and avoids a CGO setns from the shim's Go
// runtime. TODO: replace nsenter with a native setns(CLONE_NEWNET) so the
// runtime carries no external dependency.
func SpawnVMISidecar(id string, vmmPid int, cfg VMIConfig) (*VMISidecar, error) {
	if !cfg.Introspect {
		return nil, nil
	}
	if cfg.Kernel == "" {
		return nil, fmt.Errorf("vmi: no %s annotation; the control-socket kernel view needs a guest vmlinux, skipping", AnnotVMIKernel)
	}
	if _, err := os.Stat(cfg.SidecarBin); err != nil {
		return nil, fmt.Errorf("vmi: sidecar binary %q not found: %w", cfg.SidecarBin, err)
	}

	sock := filepath.Join("/proc", strconv.Itoa(vmmPid), "root", controlSockName)
	if err := waitForControlSock(sock, controlSockWait); err != nil {
		return nil, fmt.Errorf("vmi: control socket %q did not appear: %w", sock, err)
	}

	out, err := openVMIOutput(id, cfg)
	if err != nil {
		return nil, fmt.Errorf("vmi: cannot open output sink: %w", err)
	}

	args := []string{
		"-t", strconv.Itoa(vmmPid), "-n", "--",
		cfg.SidecarBin,
		"--pid", strconv.Itoa(vmmPid),
		"--tap", cfg.Tap,
		"--control-sock", sock,
		"--kernel", cfg.Kernel,
		"--gpa", cfg.GPA,
		"--emit", cfg.Emit,
		"--schema", cfg.Schema,
	}
	// The filesystem source needs the guest's block device. Annotation wins;
	// otherwise resolve it from the VMM's fds. Absent either, run without it.
	disk := cfg.Disk
	if disk == "" {
		disk = resolveVMIDisk(vmmPid)
	}
	if disk != "" {
		args = append(args, "--disk", disk)
	}
	// #nosec G204 -- args are runtime-config annotations, not attacker input.
	cmd := exec.Command("nsenter", args...)
	cmd.Stdout = out
	cmd.Stderr = out
	// New session so a signal to the shim group does not race the reaper.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		_ = out.Close()
		return nil, fmt.Errorf("vmi: failed to start sidecar: %w", err)
	}

	v := &VMISidecar{id: id, cmd: cmd, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait() // reap when it exits (VMM gone, crash, or our Stop)
		_ = out.Close()
		close(v.done)
	}()
	logrus.WithFields(logrus.Fields{
		"container": id, "vmm_pid": vmmPid, "sidecar_pid": cmd.Process.Pid,
		"tap": cfg.Tap, "kernel": cfg.Kernel, "disk": disk,
	}).Info("vmi: sidecar attached")
	return v, nil
}

// Stop signals the sidecar and reaps it, escalating to SIGKILL after a grace
// period. Safe to call on a nil receiver.
func (v *VMISidecar) Stop() {
	if v == nil || v.cmd == nil || v.cmd.Process == nil {
		return
	}
	_ = v.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-v.done:
	case <-time.After(sidecarStopGrace):
		_ = v.cmd.Process.Kill()
		<-v.done
	}
	logrus.WithField("container", v.id).Info("vmi: sidecar stopped")
}

// waitForControlSock polls until the VMM's control socket exists or timeout.
func waitForControlSock(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if fi, err := os.Stat(path); err == nil && fi.Mode()&os.ModeSocket != 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s", timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// openVMIOutput opens (creating dirs) the NDJSON sink for a container, defaulting
// to a per-container file under defaultVMIOutputDir when no path is configured.
func openVMIOutput(id string, cfg VMIConfig) (*os.File, error) {
	path := cfg.Output
	if path == "" {
		path = filepath.Join(defaultVMIOutputDir, id+".ndjson")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640) // #nosec G302
}
