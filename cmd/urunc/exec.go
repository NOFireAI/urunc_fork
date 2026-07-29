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

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v3"
	"golang.org/x/sys/unix"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	"github.com/urunc-dev/urunc/pkg/agentproto"
	"github.com/urunc-dev/urunc/pkg/unikontainers"
)

const (
	execProxyEnvFlag = "_URUNC_EXEC_PROXY"
	execProxyEnvSpec = "_URUNC_EXEC_SPEC"
)

// execProxySpec carries everything the detached proxy process needs; it is
// passed through the environment because the shim deletes process.json as
// soon as the CLI returns.
type execProxySpec struct {
	Process       specs.Process `json:"process"`
	AgentSockPath string        `json:"agentSockPath"`
	ConsoleSocket string        `json:"consoleSocket"`
}

var execCommand = &cli.Command{
	Name:  "exec",
	Usage: "execute a new process inside a container",
	ArgsUsage: `[command options] <container-id> [command...]

Where "<container-id>" is the name for the instance of the container.`,
	// We parse the runc-style flags ourselves: for non-unikernel containers
	// the whole invocation is re-executed as runc, which understands the
	// full set of "runc exec" options.
	SkipFlagParsing: true,
	Action: func(_ context.Context, cmd *cli.Command) error {
		if sock := os.Getenv(execMuxEnvFlag); sock != "" {
			return runAgentMux(sock)
		}
		if os.Getenv(execProxyEnvFlag) == "1" {
			return runExecProxy()
		}
		logrus.WithField("command", "EXEC").WithField("args", os.Args).Debug("urunc INVOKED")

		opts, err := parseExecArgs(cmd.Args().Slice())
		if err != nil {
			return err
		}
		if opts.containerID == "" {
			return errors.New("exec: missing container id")
		}

		// We have already made sure in main.go that root is not nil
		rootDir := cmd.String("root")
		_, err = unikontainers.Get(opts.containerID, rootDir)
		if err != nil {
			if errors.Is(err, unikontainers.ErrNotUnikernel) || errors.Is(err, os.ErrNotExist) {
				// Exec runc to handle non unikernel containers
				// It should never return
				return runcExec()
			}
			return err
		}

		spec := execProxySpec{
			AgentSockPath: filepath.Join(rootDir, opts.containerID, "agent.sock"),
			ConsoleSocket: opts.consoleSocket,
		}
		switch {
		case opts.processPath != "":
			data, err := os.ReadFile(opts.processPath)
			if err != nil {
				return fmt.Errorf("exec: read process spec: %w", err)
			}
			if err := json.Unmarshal(data, &spec.Process); err != nil {
				return fmt.Errorf("exec: parse process spec: %w", err)
			}
		case len(opts.directArgs) > 0:
			spec.Process = specs.Process{Args: opts.directArgs, Terminal: opts.tty}
		default:
			return errors.New("exec: no command specified")
		}

		encoded, err := json.Marshal(spec)
		if err != nil {
			return err
		}
		env := append(os.Environ(),
			execProxyEnvFlag+"=1",
			execProxyEnvSpec+"="+base64.StdEncoding.EncodeToString(encoded))

		if !opts.detach {
			os.Setenv(execProxyEnvSpec, base64.StdEncoding.EncodeToString(encoded))
			return runExecProxy()
		}

		self, err := os.Executable()
		if err != nil {
			return err
		}
		// The caller tears the exec IO down once this process exits, so the
		// proxy has to be holding the session by then: it reports readiness
		// over an extra pipe, which it closes once the guest has accepted
		// the session.
		readyR, readyW, err := os.Pipe()
		if err != nil {
			return err
		}
		proxy := exec.Command(self, "exec")
		proxy.Env = env
		proxy.Stdin = os.Stdin
		proxy.Stdout = os.Stdout
		proxy.Stderr = os.Stderr
		proxy.ExtraFiles = []*os.File{readyW}
		proxy.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := proxy.Start(); err != nil {
			_ = readyR.Close()
			_ = readyW.Close()
			return fmt.Errorf("exec: start proxy: %w", err)
		}
		_ = readyW.Close()
		// One byte, not EOF: the pipe reaches other processes the proxy
		// starts (the multiplexer), so waiting for every writer to close
		// would wait for them too.
		var ready [1]byte
		_, _ = io.ReadFull(readyR, ready[:])
		_ = readyR.Close()
		if opts.pidFile != "" {
			pidData := []byte(strconv.Itoa(proxy.Process.Pid))
			if err := os.WriteFile(opts.pidFile, pidData, 0o644); err != nil {
				return fmt.Errorf("exec: write pid file: %w", err)
			}
		}
		// The proxy lives for the whole exec session and is reparented to
		// the shim (a subreaper), which collects its exit status.
		return proxy.Process.Release()
	},
}

type execOpts struct {
	containerID   string
	processPath   string
	consoleSocket string
	pidFile       string
	detach        bool
	tty           bool
	directArgs    []string
}

// parseExecArgs understands the runc exec CLI surface well enough to bridge
// it: value-carrying flags are consumed with their argument, boolean flags
// are consumed alone, the first positional argument is the container id and
// any further positionals form a direct command (interactive use).
func parseExecArgs(args []string) (execOpts, error) {
	var opts execOpts
	valueFlags := map[string]*string{
		"--process":        &opts.processPath,
		"--console-socket": &opts.consoleSocket,
		"--pid-file":       &opts.pidFile,
	}
	ignoredValueFlags := map[string]bool{
		"--user": true, "-u": true, "--cwd": true, "--env": true, "-e": true,
		"--apparmor": true, "--process-label": true, "--preserve-fds": true,
		"--cgroup": true,
	}
	boolFlags := map[string]bool{
		"--detach": true, "-d": true, "--tty": true, "-t": true,
		"--no-new-privs": true, "--no-subreaper": true, "--ignore-paused": true,
		"--no-new-keyring": true,
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if opts.containerID != "" {
			// everything after the container id is the command
			opts.directArgs = args[i:]
			break
		}
		switch {
		case !strings.HasPrefix(arg, "-"):
			opts.containerID = arg
		case boolFlags[arg]:
			if arg == "--detach" || arg == "-d" {
				opts.detach = true
			}
			if arg == "--tty" || arg == "-t" {
				opts.tty = true
			}
		default:
			name, value, hasValue := strings.Cut(arg, "=")
			if dst, ok := valueFlags[name]; ok {
				if hasValue {
					*dst = value
					continue
				}
				if i+1 >= len(args) {
					return opts, fmt.Errorf("exec: flag %s needs a value", name)
				}
				i++
				*dst = args[i]
				continue
			}
			if ignoredValueFlags[name] && !hasValue {
				i++ // skip the flag's value too
			}
			// unknown flags are ignored: the agent session is built from
			// the process spec, which carries the authoritative data
		}
	}
	return opts, nil
}

// runExecProxy is the long-lived host end of an exec session: it bridges the
// stdio handed over by the shim to an urunit-agent session inside the guest
// and exits with the remote exit code.
func runExecProxy() error {
	encoded := os.Getenv(execProxyEnvSpec)
	if encoded == "" {
		return errors.New("exec proxy: missing spec")
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return err
	}
	var spec execProxySpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return err
	}

	stdin := os.Stdin
	stdout := os.Stdout
	stderr := os.Stderr

	// For terminal sessions the shim expects the master end of a host
	// pseudoterminal over the console socket; we keep the slave end and
	// pump it from/to the guest session, which allocates its own pty.
	if spec.Process.Terminal && spec.ConsoleSocket != "" {
		ptm, pts, err := pty.Open()
		if err != nil {
			return fmt.Errorf("exec proxy: open pty: %w", err)
		}
		conn, err := net.Dial("unix", spec.ConsoleSocket)
		if err != nil {
			return fmt.Errorf("exec proxy: dial console socket: %w", err)
		}
		uc, ok := conn.(*net.UnixConn)
		if !ok {
			return errors.New("exec proxy: console socket is not unix")
		}
		oob := unix.UnixRights(int(ptm.Fd()))
		if _, _, err := uc.WriteMsgUnix([]byte(ptm.Name()), oob, nil); err != nil {
			return fmt.Errorf("exec proxy: send console fd: %w", err)
		}
		_ = uc.Close()
		_ = ptm.Close()
		stdin = pts
		stdout = pts
		stderr = pts
	}

	conn, err := dialAgentMux(spec.AgentSockPath, 30*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()

	var wmu sync.Mutex
	writeFrame := func(t byte, stream uint32, payload []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		return agentproto.WriteFrame(conn, t, stream, payload)
	}
	writeJSON := func(t byte, stream uint32, v any) error {
		payload, err := json.Marshal(v)
		if err != nil {
			return err
		}
		return writeFrame(t, stream, payload)
	}

	// The multiplexer rewrites this onto a container-unique id, so the
	// proxy can keep its own numbering simple.
	const stream = 1
	user := ""
	if spec.Process.User.UID != 0 || spec.Process.User.GID != 0 {
		user = fmt.Sprintf("%d:%d", spec.Process.User.UID, spec.Process.User.GID)
	}
	open := agentproto.OpenRequest{
		Argv: spec.Process.Args,
		Env:  spec.Process.Env,
		Cwd:  spec.Process.Cwd,
		User: user,
		TTY:  spec.Process.Terminal,
		Rows: 24,
		Cols: 80,
	}
	if err := writeJSON(agentproto.TypeOpen, stream, open); err != nil {
		return fmt.Errorf("exec proxy: open session: %w", err)
	}
	// Tell the launcher the session is live (see the ready pipe above).
	if ready := os.NewFile(3, "ready"); ready != nil {
		_, _ = ready.Write([]byte{1})
		_ = ready.Close()
	}

	// Forward termination requests to the guest process instead of dying:
	// the session ends when the guest reports the exit code.
	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		for sig := range sigCh {
			s, ok := sig.(syscall.Signal)
			if !ok {
				continue
			}
			_ = writeJSON(agentproto.TypeSignal, stream, agentproto.Signal{Signal: int(s)})
		}
	}()

	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := stdin.Read(buf)
			if n > 0 {
				if werr := writeFrame(agentproto.TypeStdin, stream, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				_ = writeFrame(agentproto.TypeCloseStdin, stream, nil)
				return
			}
		}
	}()

	for {
		f, err := agentproto.ReadFrame(conn)
		if err != nil {
			return fmt.Errorf("exec proxy: %w: %w", errMuxGone, err)
		}
		if f.Stream != stream {
			continue
		}
		switch f.Type {
		case agentproto.TypeStdout:
			_, _ = stdout.Write(f.Payload)
		case agentproto.TypeStderr:
			_, _ = stderr.Write(f.Payload)
		case agentproto.TypeExit:
			var ex agentproto.Exit
			_ = json.Unmarshal(f.Payload, &ex)
			os.Exit(ex.Code)
		case agentproto.TypeError:
			var em agentproto.Error
			_ = json.Unmarshal(f.Payload, &em)
			return fmt.Errorf("exec proxy: agent error: %s", em.Message)
		}
	}
}

// dialAgent connects to the agent socket and waits until the in-guest agent
// actually answers: the QEMU chardev accepts connections even before the
// guest side is up, so a throwaway probe session is used as a liveness
// check before the real one opens.
func dialAgent(path string, timeout time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.Dial("unix", path)
		if err != nil {
			lastErr = err
			time.Sleep(300 * time.Millisecond)
			continue
		}
		if probeAgent(conn) {
			return conn, nil
		}
		_ = conn.Close()
		lastErr = errors.New("guest agent not answering yet")
		time.Sleep(300 * time.Millisecond)
	}
	return nil, fmt.Errorf("exec: guest agent unreachable at %s (does the image ship /urunit-agent?): %w", path, lastErr)
}

func probeAgent(conn net.Conn) bool {
	const probeStream = 0xfffffffe
	probe := agentproto.OpenRequest{Argv: []string{"/bin/sh", "-c", ":"}}
	payload, _ := json.Marshal(probe)
	if err := agentproto.WriteFrame(conn, agentproto.TypeOpen, probeStream, payload); err != nil {
		return false
	}
	_ = conn.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
	defer conn.SetReadDeadline(time.Time{})
	for {
		f, err := agentproto.ReadFrame(conn)
		if err != nil {
			return false
		}
		if f.Stream != probeStream {
			continue
		}
		if f.Type == agentproto.TypeExit || f.Type == agentproto.TypeError {
			return true
		}
	}
}
