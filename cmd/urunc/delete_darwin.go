//go:build darwin
// +build darwin

package main

import (
	"context"
	"fmt"

	"github.com/urfave/cli/v3"
)

var deleteCommand = &cli.Command{
	Name:        "delete",
	Usage:       "delete (not supported on darwin)",
	ArgsUsage:   "<container-id>",
	Description: "The delete command is not supported on macOS.",
	Action: func(_ context.Context, cmd *cli.Command) error {
		return fmt.Errorf("delete command is not supported on darwin")
	},
}
