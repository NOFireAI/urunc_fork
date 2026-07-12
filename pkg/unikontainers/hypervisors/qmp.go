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
	"fmt"
	"net"
	"time"
)

// qmpClient is a minimal QMP (QEMU Machine Protocol) client over a unix
// socket, covering the small command set snapshot/restore needs: stop, cont,
// migrate, query-migrate and query-status.
type qmpClient struct {
	conn net.Conn
	rd   *bufio.Reader
}

// qmpMessage is the union of the QMP server message shapes we care about.
type qmpMessage struct {
	QMP    json.RawMessage `json:"QMP,omitempty"`
	Return json.RawMessage `json:"return,omitempty"`
	Event  string          `json:"event,omitempty"`
	Error  *struct {
		Class string `json:"class"`
		Desc  string `json:"desc"`
	} `json:"error,omitempty"`
}

// dialQMP connects to the QMP socket, consumes the greeting and negotiates
// capabilities. The timeout bounds the whole client session (all subsequent
// commands), not just the dial.
func dialQMP(sockPath string, timeout time.Duration) (*qmpClient, error) {
	conn, err := net.DialTimeout("unix", sockPath, timeout)
	if err != nil {
		return nil, fmt.Errorf("failed to connect to QMP socket %s: %w", sockPath, err)
	}
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		conn.Close()
		return nil, err
	}
	client := &qmpClient{conn: conn, rd: bufio.NewReader(conn)}

	// The server speaks first with a greeting message.
	greeting, err := client.readMessage()
	if err != nil {
		client.close()
		return nil, fmt.Errorf("failed to read QMP greeting: %w", err)
	}
	if greeting.QMP == nil {
		client.close()
		return nil, fmt.Errorf("unexpected QMP greeting")
	}

	if _, err := client.execute("qmp_capabilities", nil); err != nil {
		client.close()
		return nil, fmt.Errorf("QMP capabilities negotiation failed: %w", err)
	}
	return client, nil
}

func (q *qmpClient) close() {
	_ = q.conn.Close()
}

func (q *qmpClient) readMessage() (*qmpMessage, error) {
	line, err := q.rd.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	var message qmpMessage
	if err := json.Unmarshal(line, &message); err != nil {
		return nil, fmt.Errorf("failed to parse QMP message %q: %w", string(line), err)
	}
	return &message, nil
}

// execute sends a QMP command and returns its "return" payload, skipping any
// asynchronous events the server interleaves.
func (q *qmpClient) execute(cmd string, args map[string]any) (json.RawMessage, error) {
	request := map[string]any{"execute": cmd}
	if args != nil {
		request["arguments"] = args
	}
	data, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	if _, err := q.conn.Write(append(data, '\n')); err != nil {
		return nil, fmt.Errorf("failed to send QMP command %s: %w", cmd, err)
	}

	for {
		message, err := q.readMessage()
		if err != nil {
			return nil, fmt.Errorf("failed to read QMP response for %s: %w", cmd, err)
		}
		switch {
		case message.Error != nil:
			return nil, fmt.Errorf("QMP command %s failed: %s: %s",
				cmd, message.Error.Class, message.Error.Desc)
		case message.Return != nil:
			return message.Return, nil
		case message.Event != "":
			// Asynchronous event (e.g. STOP, MIGRATION); not the
			// response to our command.
			continue
		default:
			return nil, fmt.Errorf("unexpected QMP message while waiting for %s response", cmd)
		}
	}
}

// waitMigrationCompleted polls query-migrate until the migration reaches a
// terminal state.
func (q *qmpClient) waitMigrationCompleted() error {
	for {
		ret, err := q.execute("query-migrate", nil)
		if err != nil {
			return err
		}
		var status struct {
			Status    string `json:"status"`
			ErrorDesc string `json:"error-desc"`
		}
		if err := json.Unmarshal(ret, &status); err != nil {
			return fmt.Errorf("failed to parse query-migrate response: %w", err)
		}
		switch status.Status {
		case "completed":
			return nil
		case "failed", "cancelled":
			return fmt.Errorf("QEMU migration %s: %s", status.Status, status.ErrorDesc)
		default:
			// setup, active, device, ...
			time.Sleep(50 * time.Millisecond)
		}
	}
}

// waitRunState polls query-status until the VM reaches the wanted run state.
func (q *qmpClient) waitRunState(want string) error {
	for {
		ret, err := q.execute("query-status", nil)
		if err != nil {
			return err
		}
		var status struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(ret, &status); err != nil {
			return fmt.Errorf("failed to parse query-status response: %w", err)
		}
		if status.Status == want {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
}
