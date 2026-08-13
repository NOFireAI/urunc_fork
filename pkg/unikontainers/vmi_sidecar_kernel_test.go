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

import "testing"

// Two consumers read the VMI kernel annotation and they do not always want the
// same file: the monitor boots it, and the sidecar resolves struct offsets out
// of it. Under Firecracker one ELF vmlinux serves both. Under hvi it cannot --
// the loader takes a bzImage and rejects an ELF, and the offset resolver takes
// an ELF and rejects a bzImage -- so the boot image is separable.
func TestVMIBootKernelPath(t *testing.T) {
	t.Parallel()

	const (
		vmlinux = "/kbuild/kata-vmi-kernel/vmlinux"
		bzImage = "/kbuild/kata-vmi-kernel/arch/x86/boot/bzImage"
	)

	tests := []struct {
		name        string
		annotations map[string]string
		wantBoot    string
		wantSidecar string
	}{
		{
			// The Firecracker case, and the reason this defaults rather than
			// becoming required: one file, one annotation, unchanged behaviour.
			name: "boot kernel defaults to the sidecar kernel",
			annotations: map[string]string{
				AnnotVMIIntrospect: "true",
				AnnotVMIKernel:     vmlinux,
			},
			wantBoot:    vmlinux,
			wantSidecar: vmlinux,
		},
		{
			// The hvi case: the monitor gets the bzImage, the sidecar keeps the
			// ELF. Getting this wrong is quiet -- pointing both at the bzImage
			// boots fine and emits nothing at all.
			name: "an explicit boot kernel does not disturb the sidecar",
			annotations: map[string]string{
				AnnotVMIIntrospect: "true",
				AnnotVMIKernel:     vmlinux,
				AnnotVMIBootKernel: bzImage,
			},
			wantBoot:    bzImage,
			wantSidecar: vmlinux,
		},
		{
			name: "an empty boot kernel is treated as unset",
			annotations: map[string]string{
				AnnotVMIIntrospect: "true",
				AnnotVMIKernel:     vmlinux,
				AnnotVMIBootKernel: "",
			},
			wantBoot:    vmlinux,
			wantSidecar: vmlinux,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := VMIConfigFromAnnotations(tc.annotations)
			if got := cfg.BootKernelPath(); got != tc.wantBoot {
				t.Errorf("BootKernelPath() = %q, want %q", got, tc.wantBoot)
			}
			if cfg.Kernel != tc.wantSidecar {
				t.Errorf("Kernel = %q, want %q", cfg.Kernel, tc.wantSidecar)
			}
		})
	}
}
