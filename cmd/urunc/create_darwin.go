//go:build darwin
// +build darwin

package main

import (
	"context"
	"fmt"

	"github.com/urfave/cli/v3"
)

var createCommand = &cli.Command{
	Name:        "create",
	Usage:       "create a container (not supported on darwin)",
	ArgsUsage:   "<container-id>",
	Description: "The create command is not supported on macOS. Use 'run' instead.",
	Action: func(_ context.Context, cmd *cli.Command) error {
		return fmt.Errorf("create command is not supported on darwin; use 'run' instead")
	},
}
