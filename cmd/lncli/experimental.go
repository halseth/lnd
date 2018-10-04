// +build experimental

package main

import (
	"context"

	"github.com/lightningnetwork/lnd/lnrpc/autopilot"
	"github.com/urfave/cli"
)

func getAutopilotClient(ctx *cli.Context) (autopilot.AutopilotClient, func()) {
	conn := getClientConn(ctx, false)

	cleanUp := func() {
		conn.Close()
	}

	return autopilot.NewAutopilotClient(conn), cleanUp
}

var autopilotStatusCommand = cli.Command{
	Name:        "autopilot-status",
	Category:    "Autopilot",
	Usage:       "",
	Description: "",
	Action:      actionDecorator(autopilotStatus),
}

func autopilotStatus(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getAutopilotClient(ctx)
	defer cleanUp()

	req := &autopilot.GetStatusRequest{}

	resp, err := client.GetStatus(ctxb, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

var autopilotEnableCommand = cli.Command{
	Name:        "autopilot-enable",
	Category:    "Autopilot",
	Usage:       "",
	Description: "",
	Flags: []cli.Flag{
		cli.BoolFlag{
			Name:  "disable",
			Usage: "",
		},
	},
	Action: actionDecorator(autopilotEnable),
}

func autopilotEnable(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getAutopilotClient(ctx)
	defer cleanUp()

	// By default we will try to enable the autopilot.
	req := &autopilot.EnableRequest{
		Enable: true,
	}

	// If the --disable flag is set, then disable autopilot instead.
	if ctx.IsSet("disable") {
		req.Enable = false
	}

	resp, err := client.Enable(ctxb, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

// extraCommands will return the set of commands to enable for experimental
// builds.
func extraCommands() []cli.Command {
	return []cli.Command{
		autopilotStatusCommand,
		autopilotEnableCommand,
	}
}
