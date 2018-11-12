// +build !autopilotrpc

package main

import "github.com/urfave/cli"

// extraCommands will return nil for non-autopilotrpc builds.
func extraCommands() []cli.Command {
	return nil
}
