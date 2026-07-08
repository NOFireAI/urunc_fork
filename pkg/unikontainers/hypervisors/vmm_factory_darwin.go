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
	"errors"
	"os"
	"os/exec"

	"github.com/sirupsen/logrus"
	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
)

const DefaultMemory uint64 = 256

type VmmType string

var ErrVMMNotInstalled = errors.New("vmm not found")
var vmmLog = logrus.WithField("subsystem", "monitors")

// NewVMM creates a new VMM instance on darwin
// Supports QEMU (with HVF acceleration) and Virtualization.framework
func NewVMM(vmmType VmmType, monitors map[string]types.MonitorConfig) (vmm types.VMM, err error) {
	defer func() {
		if err != nil {
			vmmLog.Error(err.Error())
		}
	}()

	switch vmmType {
	case QemuVmm:
		// Check if HVF is available
		if !IsHVFAvailable() {
			return nil, errors.New("HVF acceleration not available on this system (requires Apple Silicon)")
		}

		// Find QEMU binary, preferring re-signed copies that have HVF entitlements.
		// On macOS, the Homebrew QEMU binary is not ad-hoc signed with the
		// com.apple.security.hypervisor entitlement, so HVF silently fails.
		// Users typically re-sign and place a copy in /tmp/ or /usr/local/bin/.
		binaryPath := ""
		for _, candidate := range []string{
			"/tmp/qemu-system-aarch64",
			"/usr/local/bin/qemu-system-aarch64",
		} {
			if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
				binaryPath = candidate
				break
			}
		}
		if binaryPath == "" {
			// Fall back to PATH lookup (Homebrew etc.)
			var err error
			binaryPath, err = exec.LookPath("qemu-system-aarch64")
			if err != nil {
				return nil, errors.New("qemu-system-aarch64 not found in PATH; install with: brew install qemu")
			}
		}
		vmmLog.Debugf("Using QEMU binary: %s", binaryPath)

		return NewQemuDarwin(binaryPath), nil

	case VzVmm:
		// Virtualization.framework backend
		vz := NewVzDarwin()
		if err := vz.Ok(); err != nil {
			return nil, err
		}
		return vz, nil

	default:
		return nil, errors.New("unsupported VMM type on darwin: " + string(vmmType))
	}
}
