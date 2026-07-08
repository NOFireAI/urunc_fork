//go:build darwin
// +build darwin

package main

import (
	"context"
	"fmt"

	"github.com/urfave/cli/v3"
)

var killCommand = &cli.Command{
	Name:        "kill",
	Usage:       "kill (not supported on darwin)",
	ArgsUsage:   "<container-id> [signal]",
	Description: "The kill command is not supported on macOS.",
	Action: func(_ context.Context, cmd *cli.Command) error {
		return fmt.Errorf("kill command is not supported on darwin")
	},
}
