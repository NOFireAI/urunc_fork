//go:build darwin
// +build darwin

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

// Darwin/macOS shim for urunc with containerd integration
// Enables: nerdctl run --runtime urunc <image>
// This is a minimal shim that delegates to the standalone urunc CLI
package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/sirupsen/logrus"
)

var log = logrus.WithField("component", "shim-darwin")

type Container struct {
	ID       string
	Bundle   string
	Process  *os.Process
	ExitCode int
}

var (
	containers = make(map[string]*Container)
	mu         sync.Mutex
)

func main() {
	// Setup logging to file for debugging
	logrus.SetFormatter(&logrus.JSONFormatter{})
	logrus.SetLevel(logrus.DebugLevel)

	logFile, _ := os.OpenFile("/tmp/urunc-shim.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if logFile != nil {
		defer logFile.Close()
		logrus.SetOutput(logFile)
	}

	// Find urunc binary
	uruncPath := "/Users/ananos/develop/urunc-macosX/dist/urunc_static_arm64"
	if env := os.Getenv("URUNC_PATH"); env != "" {
		uruncPath = env
	}

	log.WithField("urunc", uruncPath).Info("starting shim")

	// containerd v2 shim protocol:
	// - Shim is started as a process by containerd
	// - Initial stdin contains OCI spec or config
	// - Shim maintains container state and serves requests from containerd

	// For simplicity on darwin, we implement a basic stdio protocol
	// Real implementation would use a more sophisticated IPC mechanism

	// Read initial config from stdin (OCI bundle path)
	var req map[string]interface{}
	dec := json.NewDecoder(os.Stdin)
	if err := dec.Decode(&req); err != nil {
		log.WithError(err).Fatal("failed to decode initial request")
	}

	// Extract container ID and bundle path
	id, _ := req["id"].(string)
	bundle, _ := req["bundle"].(string)

	log.WithFields(logrus.Fields{"id": id, "bundle": bundle}).Info("received container request")

	// Create container entry
	mu.Lock()
	containers[id] = &Container{
		ID:     id,
		Bundle: bundle,
	}
	mu.Unlock()

	// Verify bundle structure
	configPath := filepath.Join(bundle, "config.json")
	if _, err := os.Stat(configPath); err != nil {
		log.WithError(err).WithField("id", id).Fatal("config.json not found in bundle")
	}

	log.WithField("id", id).Info("bundle validated")

	// Signal ready
	readyResp := map[string]interface{}{
		"status": "ready",
		"pid":    0,
	}
	json.NewEncoder(os.Stdout).Encode(readyResp)

	// Launch urunc in background
	// The actual container execution happens asynchronously
	go func() {
		mu.Lock()
		container := containers[id]
		mu.Unlock()

		cmd := exec.Command(uruncPath, "run", "--bundle", bundle, id)
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		log.WithField("id", id).Debug("starting urunc")
		if err := cmd.Start(); err != nil {
			log.WithError(err).WithField("id", id).Error("failed to start urunc")
			return
		}

		container.Process = cmd.Process
		log.WithFields(logrus.Fields{"id": id, "pid": cmd.Process.Pid}).Info("container started")

		// Wait for process to exit
		if err := cmd.Wait(); err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				container.ExitCode = exitErr.ExitCode()
			}
			log.WithError(err).WithField("id", id).Debug("urunc exited")
		}
	}()

	// Simple command loop for containerd to send lifecycle commands
	// In a real implementation, this would be a proper RPC server
	// For now, keep the shim alive and log activities
	select {}
}
