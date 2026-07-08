//go:build darwin
// +build darwin

package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v3"
)

// Argument check types for the `checkArgs` function.
const (
	exactArgs = iota // Checks for an exact number of arguments.
	minArgs          // Checks for a minimum number of arguments.
	maxArgs          // Checks for a maximum number of arguments.
)

var ErrEmptyContainerID = errors.New("container ID can not be empty")

// checkArgs checks the number of arguments provided in the command-line context
// against the expected number, based on the specified checkType.
func checkArgs(cmd *cli.Command, expected, checkType int) error {
	var err error
	cmdName := cmd.Name

	switch checkType {
	case exactArgs:
		if cmd.NArg() != expected {
			err = fmt.Errorf("%s: %q requires exactly %d argument(s)", os.Args[0], cmdName, expected)
		}
	case minArgs:
		if cmd.NArg() < expected {
			err = fmt.Errorf("%s: %q requires a minimum of %d argument(s)", os.Args[0], cmdName, expected)
		}
	case maxArgs:
		if cmd.NArg() > expected {
			err = fmt.Errorf("%s: %q requires a maximum of %d argument(s)", os.Args[0], cmdName, expected)
		}
	}

	if err != nil {
		fmt.Printf("Incorrect Usage.\n\n")
		_ = cli.ShowCommandHelp(context.Background(), cmd, cmdName)
		return err
	}
	return nil
}

func getUnikontainer(cmd *cli.Command) (interface{}, error) {
	containerID := cmd.Args().First()
	if containerID == "" {
		return nil, ErrEmptyContainerID
	}

	return nil, errors.New("not supported on darwin")
}

func logrusToStderr() bool {
	l, ok := logrus.StandardLogger().Out.(*os.File)
	return ok && l.Fd() == os.Stderr.Fd()
}

// fatal prints the error's details if it is a libcontainer specific error type
// then exits the program with an exit status of 1.
func fatal(err error) {
	fatalWithCode(err, 1)
}

func fatalWithCode(err error, ret int) {
	// Make sure the error is written to the logger.
	logrus.Error(err)
	if !logrusToStderr() {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(ret)
}

// ShouldHonorXDGRuntimeDir reports whether the runtime should use XDG_RUNTIME_DIR
// for the default root directory.
func ShouldHonorXDGRuntimeDir() bool {
	// On macOS, we typically use /var/run/user/UID or similar
	return os.Geteuid() != 0
}

// prepareXDGRuntimeDir prepares the XDG_RUNTIME_DIR directory according to the XDG specification.
// It creates the directory with proper permissions and sets the sticky bit to prevent auto-pruning.
func prepareXDGRuntimeDir(root string) error {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("the path in $XDG_RUNTIME_DIR must be writable by the user: %w", err)
	}
	if err := os.Chmod(root, os.FileMode(0o700)|os.ModeSticky); err != nil {
		return fmt.Errorf("you should check permission of the path in $XDG_RUNTIME_DIR: %w", err)
	}
	return nil
}
