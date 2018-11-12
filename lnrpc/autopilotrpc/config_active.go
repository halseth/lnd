// +build autopilotrpc

package autopilotrpc

import (
	"github.com/lightningnetwork/lnd/autopilot"
	"github.com/lightningnetwork/lnd/macaroons"
)

// Config is the primary configuration struct for the autopilot RPC server. It
// contains all the items required for the rpc server to carry out its
// duties. The fields with struct tags are meant to be parsed as normal
// configuration options, while if able to be populated, the latter fields MUST
// also be specified.
type Config struct {
	// AutopilotMacPath is the path for the autopilot macaroon. If
	// unspecified then we assume that the macaroon will be found under the
	// network directory, named DefaultAutopilotMacFilename.
	AutopilotMacPath string `long:"autopilotmacaroonpath" description:"Path to the autopilot macaroon"`

	// NetworkDir is the main network directory wherein the autopilot rpc
	// server will find the macaroon named DefaultAutopilotMacFilename.
	NetworkDir string

	// MacService is the main macaroon service that we'll use to handle
	// authentication for the rpc server.
	MacService *macaroons.Service

	// Manager is the running autopilot manager.
	Manager *autopilot.Manager
}
