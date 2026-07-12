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
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
)

// fakeQMP is a minimal QMP server standing in for QEMU: it greets, accepts
// capability negotiation and answers the snapshot/restore command set. Each
// accepted connection is an independent QMP session.
type fakeQMP struct {
	sockPath string
	mu       sync.Mutex
	commands []string
	// sequenced responses for polled commands
	migrateStates []string
	runStates     []string
}

func startFakeQMP(t *testing.T, migrateStates, runStates []string) *fakeQMP {
	t.Helper()
	f := &fakeQMP{
		sockPath:      filepath.Join(t.TempDir(), "qmp.sock"),
		migrateStates: migrateStates,
		runStates:     runStates,
	}
	ln, err := net.Listen("unix", f.sockPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(conn)
		}
	}()
	return f
}

func (f *fakeQMP) serve(conn net.Conn) {
	defer conn.Close()
	write := func(s string) { _, _ = conn.Write([]byte(s + "\n")) }
	write(`{"QMP": {"version": {}, "capabilities": []}}`)

	rd := bufio.NewReader(conn)
	for {
		line, err := rd.ReadBytes('\n')
		if err != nil {
			return
		}
		var request struct {
			Execute string `json:"execute"`
		}
		if json.Unmarshal(line, &request) != nil {
			return
		}
		f.mu.Lock()
		f.commands = append(f.commands, request.Execute)
		f.mu.Unlock()

		switch request.Execute {
		case "stop":
			// Interleave an asynchronous event before the response,
			// to exercise event skipping in the client.
			write(`{"event": "STOP", "timestamp": {"seconds": 1, "microseconds": 0}}`)
			write(`{"return": {}}`)
		case "query-migrate":
			f.mu.Lock()
			state := f.migrateStates[0]
			if len(f.migrateStates) > 1 {
				f.migrateStates = f.migrateStates[1:]
			}
			f.mu.Unlock()
			write(`{"return": {"status": "` + state + `"}}`)
		case "query-status":
			f.mu.Lock()
			state := f.runStates[0]
			if len(f.runStates) > 1 {
				f.runStates = f.runStates[1:]
			}
			f.mu.Unlock()
			write(`{"return": {"status": "` + state + `"}}`)
		case "fail-me":
			write(`{"error": {"class": "GenericError", "desc": "boom"}}`)
		default: // qmp_capabilities, cont, migrate, ...
			write(`{"return": {}}`)
		}
	}
}

func (f *fakeQMP) captured() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commands...)
}

func TestQemuBuildExecCmdEnablesQMPSocket(t *testing.T) {
	q := &Qemu{binary: QemuBinary, binaryPath: testQemuBinary}
	cmd, err := q.BuildExecCmd(types.ExecArgs{
		UnikernelPath: testKernelPath,
		Command:       testCommand,
	}, &fakeUnikernel{})
	require.NoError(t, err)
	assert.Contains(t, strings.Join(cmd, " "), "-qmp unix:"+InNsAPISockPath+",server=on,wait=off")
}

func TestQemuBuildRestoreCmd(t *testing.T) {
	q := &Qemu{binary: QemuBinary, binaryPath: testQemuBinary}
	args := types.ExecArgs{
		UnikernelPath: testKernelPath,
		Command:       testCommand,
		Net:           types.NetDevParams{TapDev: "tap0_urunc", MAC: "aa:bb:cc:dd:ee:ff", MTU: 1500},
	}
	cmd, err := q.BuildRestoreCmd(args, &fakeUnikernel{}, InNsSnapshotDir)
	require.NoError(t, err)
	joined := strings.Join(cmd, " ")
	// The restore command is the full boot command line...
	assert.Contains(t, joined, "-kernel "+testKernelPath)
	assert.Contains(t, joined, "ifname=tap0_urunc")
	// ...extended with the incoming migration source, loaded paused.
	assert.Contains(t, joined, "-incoming file:"+InNsSnapshotDir+"/"+QemuSnapshotStateFile)
	assert.Contains(t, cmd, "-S")
}

func TestQemuSnapshotQMPSequence(t *testing.T) {
	q := &Qemu{binary: QemuBinary, binaryPath: testQemuBinary}
	qmp := startFakeQMP(t,
		[]string{"active", "completed"}, // query-migrate poll sequence
		[]string{"inmigrate", "paused"}, // query-status poll sequence
	)

	require.NoError(t, q.PauseVM(qmp.sockPath))
	require.NoError(t, q.SnapshotVM(qmp.sockPath, InNsSnapshotDir))
	require.NoError(t, q.ResumeVM(qmp.sockPath))
	require.NoError(t, q.FinishRestore(qmp.sockPath, InNsSnapshotDir, NetOverride{}))

	commands := qmp.captured()
	// Each operation is its own session, so capabilities are negotiated
	// per operation; filter them out to assert the command flow.
	var flow []string
	for _, command := range commands {
		if command != "qmp_capabilities" {
			flow = append(flow, command)
		}
	}
	assert.Equal(t, []string{
		"stop",
		"migrate", "query-migrate", "query-migrate",
		"cont",
		"query-status", "query-status", "cont",
	}, flow)
}

func TestQemuMigrationFailureSurfaces(t *testing.T) {
	q := &Qemu{binary: QemuBinary, binaryPath: testQemuBinary}
	qmp := startFakeQMP(t, []string{"failed"}, []string{"paused"})

	err := q.SnapshotVM(qmp.sockPath, InNsSnapshotDir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "migration failed")
}
