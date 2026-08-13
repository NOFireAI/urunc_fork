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
	"strings"
	"testing"
)

// The telemetry export region is written by the guest producer whether or not
// the guest reserved it, so an introspecting boot line MUST carve it out of the
// usable memory map. Without it the page allocator hands those pages to
// processes and the producer corrupts them.
func TestVMIIntrospectReservesTelemetryRegion(t *testing.T) {
	l := &Linux{
		App:           "/init",
		Command:       "bash -lc claude",
		Monitor:       "firecracker",
		RootFsType:    "block",
		VMIInit:       true,
		VMIIntrospect: true,
	}
	cmd, err := l.CommandString()
	if err != nil {
		t.Fatalf("CommandString: %v", err)
	}
	if !strings.Contains(cmd, "memmap=64M$0x40000000") {
		t.Errorf("introspecting boot line does not reserve the telem region:\n%s", cmd)
	}
	// The customer argv must stay last on the line.
	if i, j := strings.Index(cmd, "memmap="), strings.Index(cmd, "rdinit="); i > j {
		t.Errorf("memmap must precede rdinit=:\n%s", cmd)
	}
	t.Logf("introspect=true boot line: %s", cmd)
}

func TestNoIntrospectNoMemmap(t *testing.T) {
	l := &Linux{
		App:        "/bin/sh",
		Monitor:    "firecracker",
		RootFsType: "block",
	}
	cmd, err := l.CommandString()
	if err != nil {
		t.Fatalf("CommandString: %v", err)
	}
	if strings.Contains(cmd, "memmap=") {
		t.Errorf("non-introspecting boot line must not reserve memory:\n%s", cmd)
	}
	t.Logf("introspect=false boot line: %s", cmd)
}
