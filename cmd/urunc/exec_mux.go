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
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/urunc-dev/urunc/pkg/agentproto"
)

// The guest agent speaks over a virtio-serial port, and QEMU bridges that
// port to a single host peer at a time. Every exec would otherwise open its
// own connection, so back-to-back execs race on the reconnect and lose
// frames. The mux takes the one upstream connection for the lifetime of the
// container and lets any number of exec proxies share it: the protocol
// already multiplexes on stream ids, so the mux only has to hand out unique
// ids and route frames back to the right client.

const execMuxEnvFlag = "_URUNC_EXEC_MUX"

// clientStream is the stream id an exec proxy uses for its own session; the
// mux maps it onto a container-unique id upstream.
const clientStream = 1

// muxSocketPath is the rendezvous point between exec proxies and the mux.
// It sits next to the agent socket link in the container state directory.
func muxSocketPath(agentSockPath string) string {
	return filepath.Join(filepath.Dir(agentSockPath), "agent-mux.sock")
}

// dialAgentMux returns a connection to the guest agent multiplexer, starting
// it if this is the first exec of the container.
func dialAgentMux(agentSockPath string, timeout time.Duration) (net.Conn, error) {
	muxPath := muxSocketPath(agentSockPath)
	if conn, err := net.Dial("unix", muxPath); err == nil {
		return conn, nil
	}

	if err := startAgentMux(agentSockPath); err != nil {
		return nil, err
	}

	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.Dial("unix", muxPath)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		time.Sleep(100 * time.Millisecond)
	}
	return nil, fmt.Errorf("exec: guest agent multiplexer unreachable at %s: %w", muxPath, lastErr)
}

// startAgentMux spawns the multiplexer. Losing the race against another exec
// is fine: the loser fails to bind and exits, and both dial the winner.
func startAgentMux(agentSockPath string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	mux := exec.Command(self, "exec")
	mux.Env = append(os.Environ(), execMuxEnvFlag+"="+agentSockPath)
	mux.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := mux.Start(); err != nil {
		return fmt.Errorf("exec: start agent multiplexer: %w", err)
	}
	return mux.Process.Release()
}

// runAgentMux owns the single connection to the guest agent and fans it out
// to local clients. It exits when the guest goes away.
func runAgentMux(agentSockPath string) error {
	muxPath := muxSocketPath(agentSockPath)
	if conn, err := net.Dial("unix", muxPath); err == nil {
		// Another exec won the race and is already serving.
		_ = conn.Close()
		return nil
	}
	// Nobody answered, so any socket file left behind is stale.
	_ = os.Remove(muxPath)
	listener, err := net.Listen("unix", muxPath)
	if err != nil {
		return nil
	}
	defer func() {
		_ = listener.Close()
		_ = os.Remove(muxPath)
	}()

	m := &agentMux{
		agentSockPath: agentSockPath,
		clients:       make(map[uint32]net.Conn),
	}
	if err := m.connectUpstream(30 * time.Second); err != nil {
		return err
	}

	// The upstream link is the mux's reason to exist: keep it alive for as
	// long as the container is around, re-dialing when the guest side of
	// the virtio port drops, and give up once the state directory is gone.
	go func() {
		for {
			m.pumpUpstream()
			if _, err := os.Stat(agentSockPath); err != nil {
				break
			}
			if err := m.connectUpstream(10 * time.Second); err != nil {
				break
			}
		}
		_ = listener.Close()
	}()

	var nextStream uint32
	for {
		client, err := listener.Accept()
		if err != nil {
			return nil
		}
		nextStream++
		go m.serveClient(client, nextStream)
	}
}

type agentMux struct {
	agentSockPath string

	mu       sync.Mutex
	clients  map[uint32]net.Conn
	upstream net.Conn

	wmu sync.Mutex
}

// connectUpstream (re)establishes the single connection to the guest agent.
func (m *agentMux) connectUpstream(timeout time.Duration) error {
	conn, err := dialAgent(m.agentSockPath, timeout)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.upstream = conn
	m.mu.Unlock()
	return nil
}

func (m *agentMux) up() net.Conn {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.upstream
}

// pumpUpstream routes every frame from the guest to the client that owns the
// frame's stream.
func (m *agentMux) pumpUpstream() {
	upstream := m.up()
	for {
		f, err := agentproto.ReadFrame(upstream)
		if err != nil {
			_ = upstream.Close()
			m.closeAll()
			return
		}
		m.mu.Lock()
		client := m.clients[f.Stream]
		m.mu.Unlock()
		if client == nil {
			continue
		}
		// Clients number their own single session; the mux id is an
		// internal detail, so translate it back on the way down.
		if err := agentproto.WriteFrame(client, f.Type, clientStream, f.Payload); err != nil {
			m.drop(f.Stream)
			continue
		}
		if f.Type == agentproto.TypeExit || f.Type == agentproto.TypeError {
			m.drop(f.Stream)
		}
	}
}

// serveClient forwards a client's frames upstream, forcing them onto the
// stream id the mux assigned so clients never collide.
func (m *agentMux) serveClient(client net.Conn, stream uint32) {
	m.mu.Lock()
	m.clients[stream] = client
	m.mu.Unlock()
	defer m.drop(stream)

	for {
		f, err := agentproto.ReadFrame(client)
		if err != nil {
			return
		}
		m.wmu.Lock()
		err = agentproto.WriteFrame(m.up(), f.Type, stream, f.Payload)
		m.wmu.Unlock()
		if err != nil {
			return
		}
	}
}

func (m *agentMux) drop(stream uint32) {
	m.mu.Lock()
	client := m.clients[stream]
	delete(m.clients, stream)
	m.mu.Unlock()
	if client != nil {
		_ = client.Close()
	}
}

func (m *agentMux) closeAll() {
	m.mu.Lock()
	clients := m.clients
	m.clients = make(map[uint32]net.Conn)
	m.mu.Unlock()
	for _, client := range clients {
		_ = client.Close()
	}
}

// errMuxGone reports that the guest agent connection died under the mux.
var errMuxGone = errors.New("connection to guest agent lost")
