// +build chainrpc

package chainrpc

import (
	"fmt"

	"github.com/lightningnetwork/lnd/lnrpc"
)

func init() {
	subServer := &lnrpc.SubServerDriver{
		SubServerName: subServerName,
		New: func() lnrpc.SubServer {
			return &Server{
				quit: make(chan struct{}),
			}

		},
	}

	// If the build tag is active, then we'll register ourselves as a
	// sub-RPC server within the global lnrpc package namespace.
	if err := lnrpc.RegisterSubServer(subServer); err != nil {
		panic(fmt.Sprintf("failed to register subserver driver %s: %v",
			subServerName, err))
	}
}
