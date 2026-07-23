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
	"errors"
	"fmt"
	"os"

	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v3"

	"github.com/urunc-dev/urunc/pkg/unikontainers"
)

var execCommand = &cli.Command{
	Name:  "exec",
	Usage: "execute a new process inside a container (delegated to runc for non-unikernel containers)",
	ArgsUsage: `[command options] <container-id> [command...]

Where "<container-id>" is the name for the instance of the container.`,
	// We do not parse any flags here: for non-unikernel containers the
	// whole invocation is re-executed as runc, which understands the full
	// set of "runc exec" options (--process, --detach, --pid-file, etc.).
	SkipFlagParsing: true,
	Action: func(_ context.Context, cmd *cli.Command) error {
		logrus.WithField("command", "EXEC").WithField("args", os.Args).Debug("urunc INVOKED")
		args := cmd.Args().Slice()
		if len(args) == 0 {
			return errors.New("exec: missing container id")
		}
		// The containerd shim invokes exec as
		// "urunc [global options] exec [options] <container-id>",
		// with the process spec passed through a file. Any option value
		// cannot clash with a container id, so we look up the last
		// argument that does not start with a dash.
		containerID := ""
		for i := len(args) - 1; i >= 0; i-- {
			if args[i][0] != '-' {
				containerID = args[i]
				break
			}
		}
		if containerID == "" {
			return errors.New("exec: missing container id")
		}

		// We have already made sure in main.go that root is not nil
		rootDir := cmd.String("root")
		_, err := unikontainers.Get(containerID, rootDir)
		if err != nil {
			if errors.Is(err, unikontainers.ErrNotUnikernel) || errors.Is(err, os.ErrNotExist) {
				// Exec runc to handle non unikernel containers
				// It should never return
				return runcExec()
			}
			return err
		}

		return fmt.Errorf("exec is not yet supported for unikernel containers")
	},
}
