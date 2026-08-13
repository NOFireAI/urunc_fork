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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
)

const mib = uint64(1) << 20

// The telemetry region sits at a fixed guest-physical base and the producer
// writes it unconditionally, so a guest smaller than the end of the region
// silently backs none of it: the boot succeeds, and the host reads zeros.
func TestCheckMemmapFits(t *testing.T) {
	t.Parallel()

	// What linux.go actually puts on the line: 64 MiB at 1 GiB, ending 1088 MiB.
	const realLine = "panic=-1 console=ttyS0 root=/dev/vda rw memmap=64M$0x40000000 rdinit=/init -- sh"

	tests := []struct {
		name    string
		cmdline string
		mem     uint64
		wantErr bool
	}{
		{
			// urunc's DefaultMemory is 256 MiB, so this is what a container that
			// does not ask for memory would get -- nowhere near the region.
			name:    "the default memory does not reach the region",
			cmdline: realLine,
			mem:     256 * mib,
			wantErr: true,
		},
		{
			// The boundary: the region ends at exactly 1088 MiB.
			name:    "one MiB short of the region end is refused",
			cmdline: realLine,
			mem:     1087 * mib,
			wantErr: true,
		},
		{
			name:    "exactly the region end is accepted",
			cmdline: realLine,
			mem:     1088 * mib,
		},
		{
			name:    "2G is comfortably enough",
			cmdline: realLine,
			mem:     2048 * mib,
		},
		{
			// No introspection, no reservation, no constraint.
			name:    "a line without a memmap reservation is unconstrained",
			cmdline: "panic=-1 console=ttyS0 root=/dev/vda rw",
			mem:     64 * mib,
		},
		{
			// Zero means the monitor falls back to DefaultMemory itself, so
			// there is nothing meaningful to compare against here.
			name:    "unset memory is not checked",
			cmdline: realLine,
			mem:     0,
		},
		{
			name:    "hex and suffix forms parse",
			cmdline: "memmap=0x4000000$0x40000000",
			mem:     1088 * mib,
		},
		{
			name:    "a plain byte count parses",
			cmdline: "memmap=67108864$1073741824",
			mem:     1087 * mib,
			wantErr: true,
		},
		{
			// memmap has other forms (memmap=nn@ss, nn#ss, nn!ss) that do not
			// reserve; only the $ form must constrain the guest size.
			name:    "a non-reserving memmap form is ignored",
			cmdline: "memmap=64M@0x40000000",
			mem:     256 * mib,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := CheckMemmapFits(tc.cmdline, tc.mem)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

// The check has to fire from the monitors, not just exist: this is the path a
// real container takes.
func TestBuildExecCmdRejectsAnUnbackedRegion(t *testing.T) {
	t.Parallel()

	args := types.ExecArgs{
		KernelPath:    testHviKernel,
		UnikernelPath: testHviKernel,
		Command:       "panic=-1 memmap=64M$0x40000000 rdinit=/init -- sh",
		MemSizeB:      256 * mib,
	}

	hvi := &Hvi{binaryPath: "/usr/local/bin/hvi", binary: HviBinary}
	_, err := hvi.BuildExecCmd(args, &fakeUnikernel{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "too small")

	// And it passes once the guest is big enough.
	args.MemSizeB = 2048 * mib
	_, err = hvi.BuildExecCmd(args, &fakeUnikernel{})
	require.NoError(t, err)
}
