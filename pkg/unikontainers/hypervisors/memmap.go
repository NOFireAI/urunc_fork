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
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// memmapReserve matches the `memmap=<size>$<base>` form the boot line uses to
// carve the telemetry export region out of the guest's usable memory. Sizes and
// bases take the kernel's K/M/G suffixes and may be hex.
var memmapReserve = regexp.MustCompile(`(?:^|\s)memmap=([0-9a-fA-FxX]+[KMG]?)\$([0-9a-fA-FxX]+[KMG]?)`)

// parseMemUnit parses a kernel-style size or address: an optional 0x prefix and
// an optional K/M/G suffix.
func parseMemUnit(s string) (uint64, error) {
	mult := uint64(1)
	switch {
	case strings.HasSuffix(s, "K"), strings.HasSuffix(s, "k"):
		mult, s = 1<<10, s[:len(s)-1]
	case strings.HasSuffix(s, "M"), strings.HasSuffix(s, "m"):
		mult, s = 1<<20, s[:len(s)-1]
	case strings.HasSuffix(s, "G"), strings.HasSuffix(s, "g"):
		mult, s = 1<<30, s[:len(s)-1]
	}
	base, digits := 10, s
	if l := strings.ToLower(s); strings.HasPrefix(l, "0x") {
		base, digits = 16, s[2:]
	}
	v, err := strconv.ParseUint(digits, base, 64)
	if err != nil {
		return 0, err
	}
	return v * mult, nil
}

// CheckMemmapFits reports whether every `memmap=<size>$<base>` reservation on
// the boot line falls inside a guest of memBytes.
//
// The telemetry producer memremap()s a region at a *fixed* guest-physical base
// (0x40000000, 64 MiB) and writes into it unconditionally. If the guest is
// smaller than the end of that region, the reservation is a no-op against RAM
// that does not exist: nothing faults, the boot line still looks correct, and
// the host sidecar reads the region as zeros and rejects it with
// "not a telemetry region: magic=0x0". urunc's DefaultMemory is 256 MiB, well
// under the 1088 MiB this region needs, so a container that does not ask for
// memory hits this by default.
//
// Checking the boot line rather than a hardcoded constant keeps this honest if
// the region ever moves: whatever the guest is told to reserve has to fit.
func CheckMemmapFits(cmdline string, memBytes uint64) error {
	if memBytes == 0 {
		return nil // caller falls back to DefaultMemory; nothing to check against
	}
	for _, m := range memmapReserve.FindAllStringSubmatch(cmdline, -1) {
		size, err := parseMemUnit(m[1])
		if err != nil {
			return fmt.Errorf("cannot parse memmap size %q: %w", m[1], err)
		}
		base, err := parseMemUnit(m[2])
		if err != nil {
			return fmt.Errorf("cannot parse memmap base %q: %w", m[2], err)
		}
		if end := base + size; end > memBytes {
			return fmt.Errorf(
				"guest memory %d MiB is too small for the reserved region at %#x+%d MiB: "+
					"it ends at %d MiB, so the guest would never back it. Give the container "+
					"at least %d MiB (2G is the usual choice)",
				memBytes>>20, base, size>>20, end>>20, end>>20)
		}
	}
	return nil
}
