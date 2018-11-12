// +build autopilotrpc

package autopilotrpc

import (
	"context"
	"io/ioutil"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/lightningnetwork/lnd/autopilot"
	"github.com/lightningnetwork/lnd/lnrpc"
	"google.golang.org/grpc"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

const (
	// subServerName is the name of the sub rpc server. We'll use this name
	// to register ourselves, and we also require that the main
	// SubServerConfigDispatcher instance recognize as the name of our
	subServerName = "AutopilotRPC"
)

var (
	// macaroonOps are the set of capabilities that our minted macaroon (if
	// it doesn't already exist) will have.
	macaroonOps = []bakery.Op{
		{
			Entity: "autopilot",
			Action: "read",
		},
		{
			Entity: "autopilot",
			Action: "write",
		},
	}

	// macPermissions maps RPC calls to the permissions they require.
	macPermissions = map[string][]bakery.Op{
		"/autopilotrpc.Autopilot/GetStatus": {{
			Entity: "autopilot",
			Action: "read",
		}},
		"/autopilotrpc.Autopilot/Enable": {{
			Entity: "autopilot",
			Action: "write",
		}},
	}

	// DefaultAutopilotMacFilename is the default name of the autopilot
	// macaroon that we expect to find via a file handle within the main
	// configuration file in this package.
	DefaultAutopilotMacFilename = "autopilot.macaroon"
)

// Server is a sub-server of the main RPC server: the autopilot RPC. This sub
// RPC server allows external callers to access the status of the autopilot
// currently active within lnd, as well as configuring it at runtime.
type Server struct {
	started  int32 // To be used atomically.
	shutdown int32 // To be used atomically.

	cfg *Config

	manager *autopilot.Manager
}

// A compile time check to ensure that Server fully implements the
// AutopilotServer gRPC service.
var _ AutopilotServer = (*Server)(nil)

// fileExists reports whether the named file or directory exists.
func fileExists(name string) bool {
	if _, err := os.Stat(name); err != nil {
		if os.IsNotExist(err) {
			return false
		}
	}
	return true
}

// New returns a new instance of the autopilotrpc Autopilot sub-server. We also
// return the set of permissions for the macaroons that we may create within
// this method. If the macaroons we need aren't found in the filepath, then
// we'll create them on start up. If we're unable to locate, or create the
// macaroons we need, then we'll return with an error.
func New(cfg *Config) (*Server, lnrpc.MacaroonPerms, error) {
	// If the path of the macaroon wasn't generated, then we'll assume that
	// it's found at the default network directory.
	if cfg.AutopilotMacPath == "" {
		cfg.AutopilotMacPath = filepath.Join(
			cfg.NetworkDir, DefaultAutopilotMacFilename,
		)
	}

	// Now that we know the full path of the macaroon, we can check to see
	// if we need to create it or not.
	macFilePath := cfg.AutopilotMacPath
	if !fileExists(macFilePath) && cfg.MacService != nil {
		log.Infof("Making macaroons for Autopilot RPC Server at: %v",
			macFilePath)

		// At this point, we know that the macaroon doesn't yet exist,
		// so we need to create it with the help of the main macaroon
		// service.
		mac, err := cfg.MacService.Oven.NewMacaroon(
			context.Background(), bakery.LatestVersion, nil,
			macaroonOps...,
		)
		if err != nil {
			return nil, nil, err
		}
		macBytes, err := mac.M().MarshalBinary()
		if err != nil {
			return nil, nil, err
		}
		err = ioutil.WriteFile(macFilePath, macBytes, 0644)
		if err != nil {
			os.Remove(macFilePath)
			return nil, nil, err
		}
	}

	server := &Server{
		cfg:     cfg,
		manager: cfg.Manager,
	}

	return server, macPermissions, nil
}

// Start launches any helper goroutines required for the Server to function.
//
// NOTE: This is part of the lnrpc.SubServer interface.
func (s *Server) Start() error {
	if atomic.AddInt32(&s.started, 1) != 1 {
		return nil
	}

	return s.manager.Start()
}

// Stop signals any active goroutines for a graceful closure.
//
// NOTE: This is part of the lnrpc.SubServer interface.
func (s *Server) Stop() error {
	if atomic.AddInt32(&s.shutdown, 1) != 1 {
		return nil
	}

	return s.manager.Stop()
}

// Name returns a unique string representation of the sub-server. This can be
// used to identify the sub-server and also de-duplicate them.
//
// NOTE: This is part of the lnrpc.SubServer interface.
func (s *Server) Name() string {
	return subServerName
}

// RegisterWithRootServer will be called by the root gRPC server to direct a
// sub RPC server to register itself with the main gRPC root server. Until this
// is called, each sub-server won't be able to have
// requests routed towards it.
//
// NOTE: This is part of the lnrpc.SubServer interface.
func (s *Server) RegisterWithRootServer(grpcServer *grpc.Server) error {
	// We make sure that we register it with the main gRPC server to ensure
	// all our methods are routed properly.
	RegisterAutopilotServer(grpcServer, s)

	log.Debugf("Autopilot RPC server successfully register with root gRPC " +
		"server")

	return nil
}

// GetStatus returns the current status of the autopilot agent.
//
// NOTE: Part of the AutopilotServer interface.
func (s *Server) GetStatus(ctx context.Context,
	in *GetStatusRequest) (*GetStatusResponse, error) {

	return &GetStatusResponse{
		Active: s.manager.GetStatus(),
	}, nil
}

// Enable activates the current autopilot agent, if active.
//
// NOTE: Part of the AutopilotServer interface.
func (s *Server) Enable(ctx context.Context,
	in *EnableRequest) (*EnableResponse, error) {

	var err error
	if in.Enable {
		err = s.manager.StartAgent()
	} else {
		err = s.manager.StopAgent()
	}
	return &EnableResponse{}, err
}
