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

package hypervisors

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
	"golang.org/x/sys/unix"
)

const (
	FirecrackerVmm    VmmType = "firecracker"
	FirecrackerBinary string  = "firecracker"
	FCJsonFilename    string  = "fc.json"
)

type Firecracker struct {
	binaryPath string
	binary     string
}

type FirecrackerBootSource struct {
	ImagePath  string `json:"kernel_image_path"`
	BootArgs   string `json:"boot_args"`
	InitrdPath string `json:"initrd_path,omitempty"`
}

// stagedCPUTemplatePath is the guest-relative path (inside the monitor rootfs)
// where the custom CPU template is staged; the pivot_root'd VMM reads it here.
// Kept in sync with the staging in unikontainers.stageVMIBootFiles.
const stagedCPUTemplatePath = "/cpu-template.json"

type FirecrackerMachine struct {
	VcpuCount       uint   `json:"vcpu_count"`
	MemSizeMiB      uint64 `json:"mem_size_mib"`
	Smt             bool   `json:"smt"`
	TrackDirtyPages bool   `json:"track_dirty_pages"`
	// CpuTemplate normalises the CPUID the guest sees. Without it, the default
	// CPUID passthrough can present an INCOHERENT feature set on newer hybrid
	// hosts (e.g. vaes/avx_vnni present but avx2 masked), which makes runtimes
	// that dispatch on CPUID (Bun/BoringSSL, as used by claude-code) execute an
	// instruction the vCPU faults on -> SIGILL. A static template (e.g. "T2",
	// Skylake parity) yields a coherent set. Set via URUNC_FC_CPU_TEMPLATE.
	CpuTemplate string `json:"cpu_template,omitempty"`
}

type FirecrackerDrive struct {
	DriveID   string `json:"drive_id"`
	IsRO      bool   `json:"is_read_only"`
	IsRootDev bool   `json:"is_root_device"`
	HostPath  string `json:"path_on_host"`
}

type FirecrackerNet struct {
	IfaceID  string `json:"iface_id"`
	GuestMAC string `json:"guest_mac,omitempty"`
	HostIF   string `json:"host_dev_name"`
}

type FirecrackerVSockDev struct {
	GuestCID int    `json:"guest_cid"`
	UDSPath  string `json:"uds_path"`
	VSockID  string `json:"vsock_id"`
}

type FirecrackerConfig struct {
	Source  FirecrackerBootSource `json:"boot-source"`
	Machine FirecrackerMachine    `json:"machine-config"`
	Drives  []FirecrackerDrive    `json:"drives"`
	NetIfs  []FirecrackerNet      `json:"network-interfaces,omitempty"`
	VSock   FirecrackerVSockDev   `json:"vsock,omitempty"`
	// CpuConfig is a GUEST-RELATIVE path to a custom CPU template (JSON with
	// cpuid_modifiers). The template is staged into the monitor rootfs by
	// stageVMIBootFiles, so this path resolves inside the pivot_root'd VMM's
	// view. Unlike static templates (machine-config.cpu_template), a custom
	// template is NOT gated to specific host CPU models, so it works on
	// newer/hybrid hosts. Used to normalise an incoherent guest CPUID (e.g. mask
	// vaes/avx_vnni exposed without avx2) so Bun/BoringSSL does not fault.
	CpuConfig string `json:"cpu-config,omitempty"`
}

func (fc *Firecracker) Signal(pid int, signal unix.Signal) error {
	return unix.Kill(pid, signal)
}

func (fc *Firecracker) Stop(pid int) error {
	return killProcess(pid)
}

func (fc *Firecracker) Ok() error {
	return nil
}

func (fc *Firecracker) UsesKVM() bool {
	return true
}

// SupportsSharedfs returns a bool value depending on the monitor support for shared-fs
func (fc *Firecracker) SupportsSharedfs(_ string) bool {
	return false
}

func (fc *Firecracker) Path() string {
	return fc.binaryPath
}

func (fc *Firecracker) BuildExecCmd(args types.ExecArgs, ukernel types.Unikernel) ([]string, error) {
	// FIXME: Note for getting unikernel specific options.
	// Due to the way FC operates, we have not encountered any guest specific
	// options yet. However, we need to revisit how we can use guest specific
	// options in FC, since the string return value of the Monitor related
	// functions in the unikernel interface do not integrate well with FC's
	// json configuration.
	cmdString := fc.Path() + " --no-api --config-file "
	JSONConfigFile := filepath.Join("/tmp/", FCJsonFilename)
	cmdString += JSONConfigFile
	if !args.Seccomp {
		cmdString += " --no-seccomp"
	}

	// Same trap as under hvi: a telemetry region reserved past the end of guest
	// RAM boots fine and reads back as zeros.
	if err := CheckMemmapFits(args.Command, args.MemSizeB); err != nil {
		return nil, fmt.Errorf("firecracker: %w", err)
	}

	// VM config for Firecracker
	fcMem := DefaultMemory
	if args.MemSizeB != 0 {
		fcMem = bytesToMiB(args.MemSizeB)
		// Check if memory is too small
		if fcMem == 0 {
			fcMem = DefaultMemory
		}
	}
	// NOTE: Firecracker supports only one initrd.
	// Therefore, we depend on the guest/unikernel implementation
	// to properly handle that case and concatenate the initrd
	// files if there are more than one. Hence, always give priority
	// to the initrd taken from args.
	extraMonArgs := ukernel.MonitorCli()
	initrdPath := args.InitrdPath
	if initrdPath == "" {
		initrdPath = extraMonArgs.ExtraInitrd
	}
	FCMachine := FirecrackerMachine{
		VcpuCount:       args.VCPUs,
		MemSizeMiB:      fcMem,
		Smt:             false,
		TrackDirtyPages: false,
		CpuTemplate:     os.Getenv("URUNC_FC_CPU_TEMPLATE"),
	}

	// Net config for Firecracker
	FCNet := make([]FirecrackerNet, 0)
	if args.Net.TapDev != "" {
		AnIF := FirecrackerNet{
			IfaceID:  "net1",
			GuestMAC: args.Net.MAC,
			HostIF:   args.Net.TapDev,
		}
		FCNet = append(FCNet, AnIF)
	}

	// Block config for Firecracker
	// TODO: Add support for block devices in FIrecracker
	FCDrives := make([]FirecrackerDrive, 0)

	bArgs := ukernel.MonitorBlockCli()
	for _, blockArg := range bArgs {
		aBlock := FirecrackerDrive{
			DriveID:   blockArg.ID,
			IsRO:      false,
			IsRootDev: false,
			HostPath:  blockArg.Path,
		}
		if blockArg.ID == "rootfs" {
			aBlock.IsRootDev = true
		}
		FCDrives = append(FCDrives, aBlock)
	}
	FCSource := FirecrackerBootSource{
		ImagePath:  args.UnikernelPath,
		BootArgs:   args.Command,
		InitrdPath: initrdPath,
	}

	var FCVSockDev FirecrackerVSockDev
	if args.VAccelType == "vsock" {
		FCVSockDev = FirecrackerVSockDev{
			GuestCID: args.VSockDevID,
			UDSPath:  args.VSockDevPath + "/vaccel.sock",
			VSockID:  "root",
		}
	}

	FCConfig := &FirecrackerConfig{
		Source:  FCSource,
		Machine: FCMachine,
		Drives:  FCDrives,
		NetIfs:  FCNet,
		VSock:   FCVSockDev,
	}
	// When a custom CPU template is requested, point fc at the guest-relative
	// path where stageVMIBootFiles staged it inside the monitor rootfs (the VMM
	// is pivot_root'd there, so a host path would not resolve).
	if os.Getenv("URUNC_FC_CPU_CONFIG") != "" {
		FCConfig.CpuConfig = stagedCPUTemplatePath
	}
	FCConfigJSON, err := json.Marshal(FCConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal Firecracker config: %w", err)
	}
	if err = os.WriteFile(JSONConfigFile, FCConfigJSON, 0o644); err != nil { //nolint: gosec
		return nil, fmt.Errorf("failed to save Firecracker json config: %w", err)
	}
	vmmLog.WithField("Json", string(FCConfigJSON)).Debug("Firecracker json config")

	exArgs := strings.Split(cmdString, " ")
	return exArgs, nil
}

// PreExec performs pre-execution setup. Firecracker has no special pre-exec requirements.
func (fc *Firecracker) PreExec(_ types.ExecArgs) error {
	return nil
}
