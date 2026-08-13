// Copyright (c) 2023-2026, Nubificus LTD
// SPDX-License-Identifier: Apache-2.0

package unikontainers

// VMI annotations are platform-neutral configuration keys. Linux consumes the
// full set; shared execution-context code also needs the feature gate.
const (
	AnnotVMIIntrospect         = "com.urunc.vmi.introspect"
	AnnotVMIKernel             = "com.urunc.vmi.kernel"
	AnnotVMIBootKernel         = "com.urunc.vmi.boot_kernel"
	AnnotVMIIntrospectInterval = "com.urunc.vmi.introspect_interval"
	AnnotVMIWalkBench          = "com.urunc.vmi.walk_bench"
	AnnotVMIEventsOut          = "com.urunc.vmi.events"
	AnnotVMIGPA                = "com.urunc.vmi.gpa"
	AnnotVMIEmit               = "com.urunc.vmi.emit"
	AnnotVMISchema             = "com.urunc.vmi.schema"
	AnnotVMIOutput             = "com.urunc.vmi.output"
	AnnotVMIDisk               = "com.urunc.vmi.disk"
	AnnotVMISidecarBin         = "com.urunc.vmi.sidecar_bin"
	AnnotVMITap                = "com.urunc.vmi.tap"
	AnnotVMIPayloadDir         = "com.urunc.vmi.payload_dir"
	AnnotVMIInitrd             = "com.urunc.vmi.initrd"
)
