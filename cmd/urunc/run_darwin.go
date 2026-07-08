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

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v3"
	m "github.com/urunc-dev/urunc/internal/metrics"
	"github.com/urunc-dev/urunc/pkg/qmp"
	"github.com/urunc-dev/urunc/pkg/unikontainers"
	"github.com/urunc-dev/urunc/pkg/unikontainers/hypervisors"
	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
	"github.com/urunc-dev/urunc/pkg/unikontainers/unikernels"
)

var runUsage = `<container-id>

Where "<container-id>" is your name for the instance of the container that you
are starting. The name you provide for the container instance must be unique on
your host.`

var runDescription = `The run command creates and runs a unikernel instance on macOS via QEMU with HVF acceleration.

This is a simplified standalone implementation designed for development and testing
on Apple Silicon Macs. It directly launches QEMU without the complex namespace/container
setup required on Linux.`

var runCommand = &cli.Command{
	Name:        "run",
	Usage:       "create and run a unikernel on macOS",
	ArgsUsage:   runUsage,
	Description: runDescription,
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:    "bundle",
			Aliases: []string{"b"},
			Value:   "",
			Usage:   `path to the root of the bundle directory, defaults to the current directory`,
		},
		&cli.StringFlag{
			Name:  "console-socket",
			Value: "",
			Usage: "path to an AF_UNIX socket which will receive a file descriptor referencing the master end of the console's pseudoterminal (optional)",
		},
		&cli.StringFlag{
			Name:  "pid-file",
			Value: "",
			Usage: "specify the file to write the process id to (optional)",
		},
	},
	Action: func(_ context.Context, cmd *cli.Command) error {
		logrus.WithField("command", "RUN").WithField("args", os.Args).Debug("urunc INVOKED")
		if err := checkArgs(cmd, 1, exactArgs); err != nil {
			return err
		}
		return runUnikernelDarwin(cmd)
	},
}

// runUnikernelDarwin runs a unikernel on macOS via QEMU+HVF
func runUnikernelDarwin(cmd *cli.Command) error {
	containerID := cmd.Args().First()
	metrics.SetLoggerContainerID(containerID)
	metrics.Capture(m.TS00)

	// Get bundle path
	bundlePath := cmd.String("bundle")
	if bundlePath == "" {
		var err error
		bundlePath, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("failed to get current directory: %w", err)
		}
	}

	// Verify bundle directory exists
	bundleInfo, err := os.Stat(bundlePath)
	if err != nil {
		return fmt.Errorf("bundle directory not found: %s", bundlePath)
	}
	if !bundleInfo.IsDir() {
		return fmt.Errorf("bundle path is not a directory: %s", bundlePath)
	}

	// Load OCI spec
	specPath := bundlePath + "/config.json"
	specData, err := os.ReadFile(specPath)
	if err != nil {
		return fmt.Errorf("failed to read OCI spec at %s: %w", specPath, err)
	}

	var ociSpec struct {
		Annotations map[string]string `json:"annotations"`
		Process     struct {
			Args string `json:"args"`
		} `json:"process"`
		Root struct {
			Path string `json:"path"`
		} `json:"root"`
	}
	if err := json.Unmarshal(specData, &ociSpec); err != nil {
		return fmt.Errorf("failed to parse OCI spec: %w", err)
	}

	metrics.Capture(m.TS01)

	// Load urunc config
	uruncCfg, _ := unikontainers.LoadUruncConfig(unikontainers.UruncConfigPath)
	if uruncCfg == nil {
		uruncCfg = &unikontainers.UruncConfig{
			Monitors: map[string]types.MonitorConfig{},
		}
	}

	// Determine VMM type from annotations
	vmmType := hypervisors.QemuVmm
	if vmm, ok := ociSpec.Annotations["vmm"]; ok {
		vmmType = hypervisors.VmmType(vmm)
	}

	// Create VMM instance
	vmm, err := hypervisors.NewVMM(vmmType, uruncCfg.Monitors)
	if err != nil {
		return fmt.Errorf("failed to create VMM: %w", err)
	}

	metrics.Capture(m.TS02)

	// Determine unikernel type from annotations
	unikernelType := "rumprun" // default
	if uk, ok := ociSpec.Annotations["unikernel"]; ok {
		unikernelType = uk
	}

	// Create unikernel instance
	unikernel, err := unikernels.New(unikernelType)
	if err != nil {
		return fmt.Errorf("failed to create unikernel instance: %w", err)
	}

	metrics.Capture(m.TS03)

	// Prepare unikernel params
	// On darwin, we're always using QEMU with HVF
	vmmName := "qemu-hvf"
	ukParams := types.UnikernelParams{
		CmdLine: []string{ociSpec.Process.Args},
		EnvVars: []string{},
		Monitor: vmmName,
		Rootfs: types.RootfsParams{
			Type: "initrd",
			Path: bundlePath + "/rootfs",
		},
	}

	// Initialize unikernel with params
	if err := unikernel.Init(ukParams); err != nil {
		return fmt.Errorf("failed to initialize unikernel: %w", err)
	}

	metrics.Capture(m.TS04)

	// Prepare execution arguments
	execArgs := types.ExecArgs{
		ContainerID:   containerID,
		UnikernelPath: bundlePath + "/rootfs/unikernel",
		Command:       ociSpec.Process.Args,
		MemSizeB:      uint64(uruncCfg.Monitors[vmmName].DefaultMemoryMB) * 1024 * 1024,
		VCPUs:         uruncCfg.Monitors[vmmName].DefaultVCPUs,
		VAccelType:    "", // vAccel disabled by default
		Net: types.NetDevParams{
			MAC: "52:54:00:12:34:56", // default MAC
		},
	}

	// Set defaults if not configured
	if execArgs.MemSizeB == 0 {
		execArgs.MemSizeB = 512 * 1024 * 1024 // 512 MB default
	}
	if execArgs.VCPUs == 0 {
		execArgs.VCPUs = 1
	}

	metrics.Capture(m.TS05)

	// Verify unikernel binary or disk image exists
	rootfsDir := filepath.Dir(execArgs.UnikernelPath)
	diskImagePath := filepath.Join(rootfsDir, "disk.img")

	if _, err := os.Stat(execArgs.UnikernelPath); err != nil {
		// Check if we have a disk image instead
		if _, diskErr := os.Stat(diskImagePath); diskErr != nil {
			return fmt.Errorf("neither kernel binary (%s) nor disk image (%s) found", execArgs.UnikernelPath, diskImagePath)
		}
		logrus.Debugf("Using disk image instead of kernel binary: %s", diskImagePath)
	}

	// Build QEMU command
	cmdArgs, err := vmm.BuildExecCmd(execArgs, unikernel)
	if err != nil {
		return fmt.Errorf("failed to build VMM command: %w", err)
	}

	metrics.Capture(m.TS06)

	// Pre-execution setup
	if err := vmm.PreExec(execArgs); err != nil {
		return fmt.Errorf("failed to perform pre-execution setup: %w", err)
	}

	metrics.Capture(m.TS07)

	// Launch QEMU
	qemuCmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	qemuCmd.Stdin = os.Stdin
	qemuCmd.Stdout = os.Stdout
	qemuCmd.Stderr = os.Stderr

	logrus.Debugf("Starting QEMU: %v", cmdArgs)
	if err := qemuCmd.Start(); err != nil {
		return fmt.Errorf("failed to start QEMU: %w", err)
	}

	// Write PID file if requested
	pidFile := cmd.String("pid-file")
	if pidFile != "" {
		if err := os.WriteFile(pidFile, []byte(fmt.Sprintf("%d", qemuCmd.Process.Pid)), 0644); err != nil {
			logrus.WithError(err).Warn("failed to write PID file")
		}
	}

	metrics.Capture(m.TS08)

	// Set up signal handling for graceful shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGTERM, syscall.SIGINT)

	// Get QMP socket path if available
	var qmpSocket string
	if qemuD, ok := vmm.(*hypervisors.QemuDarwin); ok {
		qmpSocket = qemuD.GetQMPSocket()
	}

	// Goroutine to handle signals
	go func() {
		<-sigChan
		logrus.Debug("Received signal, attempting graceful shutdown via QMP")

		// Try to shutdown gracefully via QMP if socket is available
		if qmpSocket != "" {
			qmpClient, err := qmp.Dial(qmpSocket, 5*time.Second)
			if err == nil {
				defer qmpClient.Close()
				if err := qmpClient.SystemPowerdown(); err != nil {
					logrus.WithError(err).Warn("QMP SystemPowerdown failed, will use SIGTERM")
					_ = qemuCmd.Process.Signal(syscall.SIGTERM)
				}
			} else {
				logrus.WithError(err).Warn("failed to connect to QMP socket, using SIGTERM")
				_ = qemuCmd.Process.Signal(syscall.SIGTERM)
			}
		} else {
			_ = qemuCmd.Process.Signal(syscall.SIGTERM)
		}
	}()

	// Wait for QEMU to exit
	metrics.Capture(m.TS09)
	err = qemuCmd.Wait()
	metrics.Capture(m.TS10)

	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			logrus.Debugf("QEMU exited with status: %v", exitErr.ExitCode())
		} else {
			logrus.WithError(err).Error("QEMU execution error")
		}
	}

	signal.Stop(sigChan)

	return nil
}
