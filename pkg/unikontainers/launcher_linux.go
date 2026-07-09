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
	"syscall"

	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
)

// linuxLauncher performs the final monitor handoff on Linux: it builds the
// monitor command, notifies the shim that the sandbox is starting, runs the
// monitor's pre-exec setup, then replaces this (reexec'd, namespaced) process
// with the monitor via syscall.Exec. This is the exact sequence that used to
// be inlined at the end of Exec — moved verbatim, not changed.
type linuxLauncher struct{}

func (linuxLauncher) launch(u *Unikontainer, vmm types.VMM, unikernel types.Unikernel, args types.ExecArgs) error {
	// Build the VMM command once and verify it can be constructed successfully.
	// This ensures we don't report the container as started if command building fails.
	execCmd, err := vmm.BuildExecCmd(args, unikernel)
	if err != nil {
		uniklog.WithError(err).Error("failed to build VMM command")
		return err
	}

	// Notify urunc start that the monitor is ready to execute.
	// We send this after BuildExecCmd succeeds to avoid reporting a container
	// as started when the VMM command cannot be built.
	err = u.SendMessage(StartSuccess)
	if err != nil {
		return err
	}

	// Perform any monitor-specific pre-exec setup (e.g., seccomp filters for HVT).
	err = vmm.PreExec(args)
	if err != nil {
		uniklog.WithError(err).Error("failed to perform pre-exec setup")
		return err
	}

	// Execute the VMM using the command we built earlier.
	uniklog.WithField("command", execCmd).Debug("Ready to execve VMM")
	return syscall.Exec(vmm.Path(), execCmd, args.Environment) //nolint: gosec
}
