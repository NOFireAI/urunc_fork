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

//go:build linux

// urunit-agent is a small in-guest agent that spawns additional processes
// (exec sessions) on behalf of the host. It speaks the agentproto framed
// protocol over one of two transports, tried in order:
//
//  1. A virtio-serial port (/dev/virtio-ports/io.urunc.agent.0) - used by
//     the QEMU backend on macOS, where the host has no vhost-vsock.
//  2. A vsock listener on port 1024 - used by the Vz backend, where
//     vz-runner bridges a host unix socket to guest vsock connections.
//
// For tty sessions the agent allocates a pseudo-terminal next to the
// workload, so the exec'd process gets full terminal semantics (raw mode,
// job control, SIGWINCH via resize frames).
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/urunc-dev/urunc/pkg/agentproto"
	"golang.org/x/sys/unix"
)

func main() {
	log.SetPrefix("urunit-agent: ")
	log.SetFlags(0)

	// The agent starts early during guest boot with a near-empty
	// environment. exec.Command resolves bare command names against the
	// agent's own PATH, so give it a sane default.
	if os.Getenv("PATH") == "" {
		_ = os.Setenv("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}

	// The agent may start before init mounts the special filesystems;
	// make sure sysfs is there for transport discovery.
	if !exists("/sys/class") {
		if err := unix.Mount("sysfs", "/sys", "sysfs", 0, ""); err != nil {
			log.Printf("mount /sys: %v", err)
		}
	}

	// Poll for the virtio-serial port first (QEMU); if it never shows up,
	// fall back to vsock (Vz). Keep retrying rather than dying: the agent
	// is started in the background early during guest boot.
	for {
		for i := 0; i < 25; i++ {
			if dev := findVirtioPort(); dev != "" {
				serveVirtioPort(dev)
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
		if err := serveVsock(); err != nil {
			log.Printf("vsock transport unavailable: %v; retrying discovery", err)
		}
	}
}

// findVirtioPort locates the agent's virtio-serial port device. The
// /dev/virtio-ports/<name> symlink is created by udev, which minimal
// guests do not run, so resolve the name through sysfs instead
// (/sys/class/virtio-ports/vportNpM/name) and fall back to the symlink.
func findVirtioPort() string {
	if link := "/dev/virtio-ports/" + agentproto.VirtioPortName; exists(link) {
		return link
	}
	entries, err := os.ReadDir("/sys/class/virtio-ports")
	if err != nil {
		return ""
	}
	for _, e := range entries {
		name, err := os.ReadFile("/sys/class/virtio-ports/" + e.Name() + "/name")
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(name)) == agentproto.VirtioPortName {
			if dev := "/dev/" + e.Name(); exists(dev) {
				return dev
			}
		}
	}
	return ""
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// serveVirtioPort serves sessions over the virtio-serial port. The port is
// a single byte stream shared with one host peer at a time. While no host
// peer is connected, guest-side reads return EOF; treat that as "not
// connected yet" and keep the same descriptor instead of tearing down,
// otherwise frames sent right after a host connect can fall into a
// close/reopen gap.
func serveVirtioPort(dev string) {
	log.Printf("serving on virtio port %s", dev)
	f, err := os.OpenFile(dev, os.O_RDWR, 0)
	if err != nil {
		log.Fatalf("open %s: %v", dev, err)
	}
	serveConn(struct {
		io.Reader
		io.Writer
	}{patientReader{f}, f})
}

// patientReader retries reads that report EOF, which on a virtio-serial
// port only means the host peer is not currently connected.
type patientReader struct {
	f *os.File
}

func (p patientReader) Read(b []byte) (int, error) {
	for {
		n, err := p.f.Read(b)
		if n > 0 {
			return n, nil
		}
		if err == nil || err == io.EOF {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		return 0, err
	}
}

// serveVsock listens on the agent vsock port and serves each connection
// concurrently.
func serveVsock() error {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("vsock socket: %w", err)
	}
	sa := &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: agentproto.DefaultVsockPort}
	if err := unix.Bind(fd, sa); err != nil {
		unix.Close(fd)
		return fmt.Errorf("vsock bind: %w", err)
	}
	if err := unix.Listen(fd, 8); err != nil {
		unix.Close(fd)
		return fmt.Errorf("vsock listen: %w", err)
	}
	log.Printf("listening on vsock port %d", agentproto.DefaultVsockPort)
	for {
		nfd, _, err := unix.Accept(fd)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			return fmt.Errorf("vsock accept: %w", err)
		}
		conn := os.NewFile(uintptr(nfd), "vsock-conn")
		go func() {
			serveConn(conn)
			conn.Close()
		}()
	}
}

// connState is one protocol peer: a frame writer shared by all sessions
// multiplexed on the connection.
type connState struct {
	mu sync.Mutex
	w  io.Writer

	sessMu   sync.Mutex
	sessions map[uint32]*session
}

func (c *connState) writeFrame(typ byte, stream uint32, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return agentproto.WriteFrame(c.w, typ, stream, payload)
}

func (c *connState) writeJSON(typ byte, stream uint32, v any) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.writeFrame(typ, stream, payload)
}

func (c *connState) session(id uint32) *session {
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	return c.sessions[id]
}

func (c *connState) addSession(id uint32, s *session) {
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	c.sessions[id] = s
}

func (c *connState) removeSession(id uint32) {
	c.sessMu.Lock()
	defer c.sessMu.Unlock()
	delete(c.sessions, id)
}

// session is one running exec'd process.
type session struct {
	cmd   *exec.Cmd
	ptmx  *os.File       // tty sessions
	stdin io.WriteCloser // non-tty sessions
}

func serveConn(rw io.ReadWriter) {
	c := &connState{w: rw, sessions: make(map[uint32]*session)}
	for {
		f, err := agentproto.ReadFrame(rw)
		if err != nil {
			if err != io.EOF {
				log.Printf("read: %v", err)
			}
			// Tear down every session still attached to this peer.
			c.sessMu.Lock()
			for _, s := range c.sessions {
				if s.cmd.Process != nil {
					_ = s.cmd.Process.Kill()
				}
			}
			c.sessMu.Unlock()
			return
		}
		switch f.Type {
		case agentproto.TypeOpen:
			var req agentproto.OpenRequest
			if err := json.Unmarshal(f.Payload, &req); err != nil {
				_ = c.writeJSON(agentproto.TypeError, f.Stream, agentproto.Error{Message: "bad open request: " + err.Error()})
				continue
			}
			if err := c.open(f.Stream, req); err != nil {
				_ = c.writeJSON(agentproto.TypeError, f.Stream, agentproto.Error{Message: err.Error()})
			}
		case agentproto.TypeStdin:
			if s := c.session(f.Stream); s != nil {
				if s.ptmx != nil {
					_, _ = s.ptmx.Write(f.Payload)
				} else if s.stdin != nil {
					_, _ = s.stdin.Write(f.Payload)
				}
			}
		case agentproto.TypeCloseStdin:
			if s := c.session(f.Stream); s != nil && s.stdin != nil {
				_ = s.stdin.Close()
			}
		case agentproto.TypeResize:
			var rs agentproto.Resize
			if err := json.Unmarshal(f.Payload, &rs); err == nil {
				if s := c.session(f.Stream); s != nil && s.ptmx != nil {
					_ = pty.Setsize(s.ptmx, &pty.Winsize{Rows: rs.Rows, Cols: rs.Cols})
				}
			}
		case agentproto.TypeSignal:
			var sg agentproto.Signal
			if err := json.Unmarshal(f.Payload, &sg); err == nil {
				if s := c.session(f.Stream); s != nil && s.cmd.Process != nil {
					_ = s.cmd.Process.Signal(syscall.Signal(sg.Signal))
				}
			}
		default:
			log.Printf("unknown frame type %d", f.Type)
		}
	}
}

// open starts the requested process on the given stream.
func (c *connState) open(stream uint32, req agentproto.OpenRequest) error {
	if len(req.Argv) == 0 {
		return fmt.Errorf("empty argv")
	}
	if c.session(stream) != nil {
		return fmt.Errorf("stream %d already in use", stream)
	}

	log.Printf("open stream %d: argv=%v tty=%v", stream, req.Argv, req.TTY)
	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	cmd.Env = defaultEnv(req.Env)
	cmd.Dir = req.Cwd
	if cmd.Dir == "" {
		cmd.Dir = "/"
	}

	s := &session{cmd: cmd}
	if req.TTY {
		ws := &pty.Winsize{Rows: req.Rows, Cols: req.Cols}
		if ws.Rows == 0 || ws.Cols == 0 {
			ws.Rows, ws.Cols = 24, 80
		}
		ptmx, err := pty.StartWithSize(cmd, ws)
		if err != nil {
			return fmt.Errorf("start (tty): %w", err)
		}
		s.ptmx = ptmx
		go func() {
			buf := make([]byte, 32*1024)
			for {
				n, err := ptmx.Read(buf)
				if n > 0 {
					if werr := c.writeFrame(agentproto.TypeStdout, stream, buf[:n]); werr != nil {
						break
					}
				}
				if err != nil {
					break
				}
			}
		}()
	} else {
		stdin, err := cmd.StdinPipe()
		if err != nil {
			return err
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			return err
		}
		stderr, err := cmd.StderrPipe()
		if err != nil {
			return err
		}
		if err := cmd.Start(); err != nil {
			return fmt.Errorf("start: %w", err)
		}
		s.stdin = stdin
		relay := func(r io.Reader, typ byte) {
			buf := make([]byte, 32*1024)
			for {
				n, err := r.Read(buf)
				if n > 0 {
					if werr := c.writeFrame(typ, stream, buf[:n]); werr != nil {
						return
					}
				}
				if err != nil {
					return
				}
			}
		}
		go relay(stdout, agentproto.TypeStdout)
		go relay(stderr, agentproto.TypeStderr)
	}

	c.addSession(stream, s)
	go func() {
		code := 0
		if err := cmd.Wait(); err != nil {
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
				if code < 0 {
					// Killed by signal: report 128+sig like a shell.
					if st, ok := ee.Sys().(syscall.WaitStatus); ok && st.Signaled() {
						code = 128 + int(st.Signal())
					}
				}
			} else {
				code = 126
			}
		}
		if s.ptmx != nil {
			_ = s.ptmx.Close()
		}
		c.removeSession(stream)
		_ = c.writeJSON(agentproto.TypeExit, stream, agentproto.Exit{Code: code})
	}()

	return nil
}

// defaultEnv fills in PATH and TERM when the request does not carry them.
func defaultEnv(env []string) []string {
	hasPath, hasTerm := false, false
	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			hasPath = true
		}
		if strings.HasPrefix(e, "TERM=") {
			hasTerm = true
		}
	}
	if !hasPath {
		env = append(env, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	if !hasTerm {
		env = append(env, "TERM=xterm-256color")
	}
	return env
}
