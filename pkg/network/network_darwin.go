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

package network

import (
	"fmt"
)

// VMNetManager implements Manager for macOS using vmnet
type VMNetManager struct {
	Mode              string // "shared" or "bridged"
	BridgeInterface   string // e.g. "en0" for bridged mode
}

// NetworkSetup configures vmnet networking for the unikernel
// On macOS, vmnet is handled by QEMU directly, so this is mostly a no-op
func (v *VMNetManager) NetworkSetup(uid uint32, gid uint32) (*UnikernelNetworkInfo, error) {
	// vmnet networking is configured at the QEMU command line level
	// This just returns placeholder network info
	return &UnikernelNetworkInfo{
		TapDevice: "", // vmnet doesn't use TAP devices
		EthDevice: Interface{
			IP:             "dynamic", // vmnet-shared provides DHCP
			DefaultGateway: "auto",
			Mask:           "auto",
			Interface:      "vmnet0",
			MAC:            "",
		},
	}, nil
}

// StubNetwork is a placeholder network manager for darwin
type StubNetwork struct{}

// NetworkSetup is a no-op for stub network
func (s *StubNetwork) NetworkSetup(uid uint32, gid uint32) (*UnikernelNetworkInfo, error) {
	return &UnikernelNetworkInfo{
		TapDevice: "",
		EthDevice: Interface{
			IP:             "0.0.0.0",
			DefaultGateway: "0.0.0.0",
			Mask:           "0.0.0.0",
			Interface:      "stub",
			MAC:            "",
		},
	}, nil
}

// StaticNetwork is a stub for darwin (static network configuration)
type StaticNetwork struct{}

// NetworkSetup is a no-op for static network on darwin
func (s *StaticNetwork) NetworkSetup(uid uint32, gid uint32) (*UnikernelNetworkInfo, error) {
	return &UnikernelNetworkInfo{
		TapDevice: "",
		EthDevice: Interface{
			IP:             "static",
			DefaultGateway: "",
			Mask:           "",
			Interface:      "static",
			MAC:            "",
		},
	}, nil
}

// DynamicNetwork is a stub for darwin (dynamic network configuration)
type DynamicNetwork struct{}

// NetworkSetup is a no-op for dynamic network on darwin
func (s *DynamicNetwork) NetworkSetup(uid uint32, gid uint32) (*UnikernelNetworkInfo, error) {
	return &UnikernelNetworkInfo{
		TapDevice: "",
		EthDevice: Interface{
			IP:             "dynamic",
			DefaultGateway: "auto",
			Mask:           "auto",
			Interface:      "vmnet0",
			MAC:            "",
		},
	}, nil
}

// CleanupAllUruncTaps is a no-op on darwin (vmnet cleanup is handled by QEMU)
func CleanupAllUruncTaps() error {
	return nil
}

// NewVMNetManager creates a vmnet-based network manager for darwin
func NewVMNetManager(mode, bridgeInterface string) (Manager, error) {
	if mode != "shared" && mode != "bridged" {
		return nil, fmt.Errorf("invalid vmnet mode: %s", mode)
	}
	return &VMNetManager{
		Mode:            mode,
		BridgeInterface: bridgeInterface,
	}, nil
}
