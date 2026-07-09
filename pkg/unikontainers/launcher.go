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

//go:build linux
// +build linux

package unikontainers

import (
	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
)

// launcher is the platform seam for the final handoff to the monitor: build
// the monitor command, notify any supervisor that the sandbox is starting,
// run monitor-specific pre-exec setup, and execute the monitor.
//
// Linux (linuxLauncher) replaces the current, reexec'd process via
// syscall.Exec and notifies the containerd shim. darwin (added in a later
// increment) spawns the monitor — or the vz-runner helper — as a child.
// Everything before this point in Exec (spec/annotation parsing, unikernel
// params, rootfs selection) is platform-neutral shared code.
type launcher interface {
	launch(u *Unikontainer, vmm types.VMM, unikernel types.Unikernel, args types.ExecArgs) error
}
