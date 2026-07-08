//go:build darwin
// +build darwin

package main

import (
	"context"
	"fmt"

	"github.com/urfave/cli/v3"
)

var startCommand = &cli.Command{
	Name:        "start",
	Usage:       "start (not supported on darwin)",
	ArgsUsage:   "<container-id>",
	Description: "The start command is not supported on macOS.",
	Action: func(_ context.Context, cmd *cli.Command) error {
		return fmt.Errorf("start command is not supported on darwin")
	},
}
