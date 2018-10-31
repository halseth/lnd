// +build signrpc

package signrpc

import (
	"fmt"

	"github.com/lightningnetwork/lnd/lnrpc"
)

// createNewSubServer is a helper method that will create the new signer sub
// server given the main config dispatcher method. If we're unable to find the
// config that is meant for us in the config dispatcher, then we'll exit with
// an error.
func createNewSubServer(c interface{}) (lnrpc.SubServer, lnrpc.MacaroonPerms, error) {

	config, ok := c.(*Config)
	if !ok {
		return nil, nil, fmt.Errorf("wrong type of config for "+
			"subserver %s, expected %T got %T", SubServerName,
			&Config{}, c)
	}

	// Before we try to make the new signer service instance, we'll perform
	// some sanity checks on the arguments to ensure that they're useable.
	switch {
	case config.MacService == nil:
		return nil, nil, fmt.Errorf("MacService must be set to create " +
			"Signrpc")
	case config.NetworkDir == "":
		return nil, nil, fmt.Errorf("NetworkDir must be set to create " +
			"Signrpc")
	case config.Signer == nil:
		return nil, nil, fmt.Errorf("Signer must be set to create " +
			"Signrpc")
	}

	return New(config)
}

func init() {
	subServer := &lnrpc.SubServerDriver{
		SubServerName: SubServerName,
		New: func(c interface{}) (lnrpc.SubServer, lnrpc.MacaroonPerms, error) {
			return createNewSubServer(c)
		},
	}

	// If the build tag is active, then we'll register ourselves as a
	// sub-RPC server within the global lnrpc package namespace.
	if err := lnrpc.RegisterSubServer(subServer); err != nil {
		panic(fmt.Sprintf("failed to register sub server driver '%s': %v",
			SubServerName, err))
	}
}
