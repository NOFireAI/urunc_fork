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

package initrd

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/cavaliergopher/cpio"
)

// vmiPayloadFiles maps the in-initrd destination path to the source file name
// (relative to the host payload dir) for the VMI telemetry injection. Paths are
// flat and "vmi-" prefixed so no cpio parent-dir entry is required and customer
// files at "/" are never shadowed. "/init" is the rdinit entrypoint (see
// packaging/vmi-initrd/vmi-init): it mounts tracefs, launches telem-capture and
// urunit-agent, then execs the customer entrypoint passed after "--".
var vmiPayloadFiles = []struct {
	dest, src string
	optional  bool
}{
	{dest: "/init", src: "init"},
	// Must be named exactly "busybox": busybox only dispatches argv[1] as the
	// applet (e.g. `busybox sh`) when argv[0]'s basename is "busybox". A renamed
	// binary makes every invocation "applet not found" (exit 127).
	{dest: "/busybox", src: "busybox"},
	{dest: "/vmi-telem-capture", src: "vmi-telem-capture"},
	{dest: "/vmi-urunit-agent", src: "vmi-urunit-agent", optional: true},
}

// AugmentInitrdForVMI appends the VMI telemetry payload from payloadDir onto an
// existing initrd (cpio newc, appended in place — the same augmentation
// mechanism CopyFileMountsToInitrd uses). This is how a stock customer image
// gains rich eBPF capture + an exec/console channel with zero image change: the
// customer rootfs-as-initrd is left intact and the telemetry init + agents are
// layered on top, with /init becoming the rdinit that eventually execs the
// customer entrypoint. Optional payload files that are absent are skipped so a
// partial payload (e.g. no urunit-agent build yet) still boots.
func AugmentInitrdForVMI(initrdPath, payloadDir string) error {
	f, err := os.OpenFile(initrdPath, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to open initrd %s for VMI augmentation: %w", initrdPath, err)
	}
	defer f.Close()

	w := cpio.NewWriter(f)
	for _, pf := range vmiPayloadFiles {
		full := filepath.Join(payloadDir, pf.src)
		if _, statErr := os.Stat(full); statErr != nil {
			if pf.optional {
				continue
			}
			return fmt.Errorf("VMI payload file missing: %s: %w", full, statErr)
		}
		if err := CopyFileToInitrd(w, full, pf.dest); err != nil {
			return fmt.Errorf("failed to append VMI payload %s: %w", pf.dest, err)
		}
	}

	return w.Close()
}
