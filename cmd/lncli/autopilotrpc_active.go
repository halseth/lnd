// +build autopilotrpc

package main

import (
	"context"

	"github.com/lightningnetwork/lnd/lnrpc/autopilotrpc"
	"github.com/urfave/cli"
)

func getAutopilotClient(ctx *cli.Context) (autopilotrpc.AutopilotClient, func()) {
	conn := getClientConn(ctx, false)

	cleanUp := func() {
		conn.Close()
	}

	return autopilotrpc.NewAutopilotClient(conn), cleanUp
}

var getStatusCommand = cli.Command{
	Name:        "autopilot-status",
	Category:    "Autopilot",
	Usage:       "",
	Description: "",
	Action:      actionDecorator(getStatus),
}

func getStatus(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getAutopilotClient(ctx)
	defer cleanUp()

	req := &autopilotrpc.GetStatusRequest{}

	resp, err := client.GetStatus(ctxb, req)
	if err != nil {
		return err
	}

	printRespJSON(resp)
	return nil
}

var enableCommand = cli.Command{
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
	Action: actionDecorator(enable),
}

func enable(ctx *cli.Context) error {
	ctxb := context.Background()
	client, cleanUp := getAutopilotClient(ctx)
	defer cleanUp()

	// By default we will try to enable the autopilot.
	req := &autopilotrpc.EnableRequest{
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

// extraCommands will return the set of commands to enable for autopilotrpc
// builds.
func extraCommands() []cli.Command {
	return []cli.Command{
		getStatusCommand,
		enableCommand,
	}
}
