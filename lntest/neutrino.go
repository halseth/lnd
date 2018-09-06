package lntest

// NeutrinoBackendConfig is an implementation of the BackendConfig interface
// backed by a neutrino node.
type NeutrinoBackendConfig struct {
	minerAddr string
}

// GenArgs returns the arguments needed to be passed to LND at startup for
// using this node as a chain backend.
func (b NeutrinoBackendConfig) GenArgs() []string {
	var args []string
	args = append(args, "--bitcoin.node=neutrino")
	args = append(args, "--neutrino.connect="+b.minerAddr)
	return args
}

// P2PAddr returns the address of this node to be used when connection over the
// Bitcoin P2P network.
func (b NeutrinoBackendConfig) P2PAddr() string {
	// TODO(halseth): must be implemented for reorg test.
	return ""
}

// NewNeutrinoBackend starts a new returns a NeutrinoBackendConfig for the node.
func NewNeutrinoBackend(miner string) (*NeutrinoBackendConfig, func(), error) {
	bd := &NeutrinoBackendConfig{
		minerAddr: miner,
	}

	cleanUp := func() {}
	return bd, cleanUp, nil
}
