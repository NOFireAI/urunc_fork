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
	"fmt"

	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/urunc-dev/urunc/pkg/unikontainers/initrd"
	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
)

// TODO: Find and set the correct size for the tmpfs in the host
const tmpfsSizeForInitrdRootfs = "65536k"

type initrdRootfs struct {
	mounts             []specs.Mount
	monRootfs          string
	initrdHostFullPath string
	// vmiIntrospect, when true, appends the VMI telemetry payload
	// (init wrapper + telem-capture + urunit-agent) to the initrd so a
	// stock customer image boots with rich eBPF + exec/console, no image
	// change. vmiPayloadDir is the host dir staged by
	// packaging/vmi-initrd/build-vmi-payload.sh.
	vmiIntrospect bool
	vmiPayloadDir string
}

func (i initrdRootfs) preSetup() error {
	return nil
}

func (i initrdRootfs) postSetup() error {
	err := initrd.CopyFileMountsToInitrd(i.initrdHostFullPath, i.mounts)
	if err != nil {
		return fmt.Errorf("failed to update guest's initrd: %w", err)
	}

	// Option C: inject the VMI telemetry payload into the (untouched) customer
	// initrd so an unmodified OCI image boots with rich eBPF + exec/console.
	if i.vmiIntrospect {
		if err := initrd.AugmentInitrdForVMI(i.initrdHostFullPath, i.vmiPayloadDir); err != nil {
			return fmt.Errorf("failed to inject VMI telemetry payload into initrd: %w", err)
		}
	}

	return nil
}

func (i initrdRootfs) getMounts() ([]specs.Mount, error) {
	return []specs.Mount{tmpfsMount("/tmp", tmpfsSizeForInitrdRootfs)}, nil
}

func (i initrdRootfs) getBlockDevs() ([]types.BlockDevParams, error) {
	return nil, nil
}

// TODO: Return an array instead of a single struct
func (i initrdRootfs) getSharedDirs() (types.SharedfsParams, error) {
	return types.SharedfsParams{}, nil
}

func (i initrdRootfs) preStart() error {
	return nil
}
