// +build !signrpc

package signrpc

import (
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/macaroons"
)

// Config is empty for non-signrpc builds.
type Config struct{}

func (c *Config) Populate(sign lnwallet.Signer,
	networkDir string, macService *macaroons.Service) *Config {
	// Intentionally left empty.
	return c
}
