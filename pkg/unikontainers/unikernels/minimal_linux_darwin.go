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

package unikernels

import (
	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
)

// MinimalLinux is a minimal Linux unikernel implementation for testing on macOS
type MinimalLinux struct {
	cmdline string
}

func newMinimalLinux() *MinimalLinux {
	return &MinimalLinux{}
}

func (m *MinimalLinux) Init(params types.UnikernelParams) error {
	if len(params.CmdLine) > 0 {
		m.cmdline = params.CmdLine[0]
	}
	return nil
}

func (m *MinimalLinux) CommandString() (string, error) {
	return m.cmdline, nil
}

func (m *MinimalLinux) SupportsBlock() bool {
	return true
}

func (m *MinimalLinux) SupportsFS(fsType string) bool {
	// Minimal Linux supports 9pfs and virtiofs
	return fsType == "9pfs" || fsType == "virtiofs"
}

func (m *MinimalLinux) MonitorNetCli(tapDev, mac string) string {
	// Use default virtio-net configuration
	return ""
}

func (m *MinimalLinux) MonitorBlockCli() []types.MonitorBlockArgs {
	return []types.MonitorBlockArgs{}
}

func (m *MinimalLinux) MonitorCli() types.MonitorCliArgs {
	return types.MonitorCliArgs{}
}
