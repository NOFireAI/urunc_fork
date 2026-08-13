//go:build darwin

package hypervisors

import (
	"strings"
	"testing"

	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
)

func TestHviDarwinGenericContainerBoot(t *testing.T) {
	hvi := NewHviDarwin("/opt/hvi")
	args := types.ExecArgs{
		ContainerID:   "alpine-test",
		KernelPath:    "/host/Image",
		InitrdPath:    "/instance/container-initrd",
		Command:       "rdinit=/vz-init console=ttyAMA0",
		MemSizeB:      512 << 20,
		VCPUs:         2,
		AgentSockPath: "/instance/agent.sock",
		Net:           types.NetDevParams{TapDev: "en0"},
		Sharedfs: types.SharedfsParams{
			Type: "virtiofs", Path: "/store/alpine/rootfs", Tag: "rootfs", ReadOnly: true,
		},
	}
	argv, err := hvi.BuildExecCmd(args, &fakeUnikernel{})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(argv, " ")
	for _, want := range []string{
		"/opt/hvi boot",
		"--kernel /host/Image",
		"--initramfs /instance/container-initrd",
		"--share-ro /store/alpine/rootfs rootfs",
		"--cmdline rdinit=/vz-init console=ttyAMA0",
		"--agent-sock /instance/agent.sock",
		"--net",
		"--sandbox-id alpine-test",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected %q in HVI command:\n%s", want, joined)
		}
	}
}

func TestHviDarwinRejectsWritableOrMultipleShares(t *testing.T) {
	hvi := NewHviDarwin("/opt/hvi")
	_, err := hvi.BuildExecCmd(types.ExecArgs{
		KernelPath: "/host/Image",
		Sharedfs:   types.SharedfsParams{Path: "/host/rootfs"},
	}, &fakeUnikernel{})
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("writable export: got %v", err)
	}
	_, err = hvi.BuildExecCmd(types.ExecArgs{
		KernelPath: "/host/Image",
		SharedDirs: []types.SharedDirParams{{Path: "/host/extra", Tag: "extra"}},
	}, &fakeUnikernel{})
	if err == nil || !strings.Contains(err.Error(), "one virtio-fs export") {
		t.Fatalf("additional export: got %v", err)
	}
}
