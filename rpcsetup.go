package lnd

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	proxy "github.com/grpc-ecosystem/grpc-gateway/runtime"

	"github.com/lightningnetwork/lnd/lncfg"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/lnwallet"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/lightningnetwork/lnd/monitoring"
	"github.com/lightningnetwork/lnd/walletunlocker"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// renamse to wallet state or similar?

type rpcState struct {
	macaroonService *macaroons.Service
	grpcServer      *grpc.Server

	tlsCfg        *tls.Config
	restCreds     *credentials.TransportCredentials
	restProxyDest string

	pwService *walletunlocker.UnlockerService
	rpcServer *rpcServer

	unaryInterceptors  []grpc.UnaryServerInterceptor
	streamInterceptors []grpc.StreamServerInterceptor
}

// UnaryServerInterceptor is a GRPC interceptor that checks whether the
// request is authorized by the included macaroons.
func (r *rpcState) UnaryStateInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (interface{}, error) {
		return handler(ctx, req)
	}
}

// StreamServerInterceptor is a GRPC interceptor that checks whether the
// request is authorized by the included macaroons.
func (r *rpcState) StreamSteateInterceptor() grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream,
		info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		return handler(srv, ss)
	}
}

func (r *rpcState) Init() error {

	// Only process macaroons if --no-macaroons isn't set.
	var err error

	r.tlsCfg, r.restCreds, r.restProxyDest, err = getTLSConfig(
		cfg.TLSCertPath, cfg.TLSKeyPath, cfg.TLSExtraIPs,
		cfg.TLSExtraDomains, cfg.RPCListeners,
	)
	if err != nil {
		err := fmt.Errorf("Unable to load TLS credentials: %v", err)
		ltndLog.Error(err)
		return err
	}

	serverCreds := credentials.NewTLS(r.tlsCfg)
	serverOpts := []grpc.ServerOption{grpc.Creds(serverCreds)}

	// If macaroons aren't disabled (a non-nil service), then we'll set up
	// our set of interceptors which will allow us to handle the macaroon
	// authentication in a single location.
	macUnaryInterceptors := []grpc.UnaryServerInterceptor{}
	macStrmInterceptors := []grpc.StreamServerInterceptor{}

	if !cfg.NoMacaroons {
		// Create the macaroon authentication/authorization service.
		var err error
		r.macaroonService, err = macaroons.NewService(
			networkDir, macaroons.IPLockChecker,
		)
		if err != nil {
			err := fmt.Errorf("Unable to set up macaroon "+
				"authentication: %v", err)
			ltndLog.Error(err)
			return err
		}
		//defer macaroonService.Close()

		permissions := mainRPCServerPermissions()
		unaryInterceptor := r.macaroonService.UnaryServerInterceptor(permissions)
		macUnaryInterceptors = append(macUnaryInterceptors, unaryInterceptor)

		strmInterceptor := r.macaroonService.StreamServerInterceptor(permissions)
		macStrmInterceptors = append(macStrmInterceptors, strmInterceptor)

	}

	// Get interceptors for Prometheus to gather gRPC performance metrics.
	// If monitoring is disabled, GetPromInterceptors() will return empty
	// slices.
	promUnaryInterceptors, promStrmInterceptors := monitoring.GetPromInterceptors()

	// Concatenate the slices of unary and stream interceptors respectively.
	unaryInterceptors := append(macUnaryInterceptors, promUnaryInterceptors...)
	strmInterceptors := append(macStrmInterceptors, promStrmInterceptors...)

	// We'll also add our logging interceptors as well, so we can
	// automatically log all errors that happen during RPC calls.
	unaryInterceptors = append(
		unaryInterceptors, errorLogUnaryServerInterceptor(rpcsLog),
	)
	strmInterceptors = append(
		strmInterceptors, errorLogStreamServerInterceptor(rpcsLog),
	)

	// If any interceptors have been set up, add them to the server options.
	if len(unaryInterceptors) != 0 && len(strmInterceptors) != 0 {
		chainedUnary := grpc_middleware.WithUnaryServerChain(
			unaryInterceptors...,
		)
		chainedStream := grpc_middleware.WithStreamServerChain(
			strmInterceptors...,
		)
		serverOpts = append(serverOpts, chainedUnary, chainedStream)
	}

	r.grpcServer = grpc.NewServer(serverOpts...)

	// We wait until the user provides a password over RPC. In case lnd is
	// started with the --noseedbackup flag, we use the default password
	// for wallet encryption.
	if !cfg.NoSeedBackup {
		chainConfig := cfg.Bitcoin
		if registeredChains.PrimaryChain() == litecoinChain {
			chainConfig = cfg.Litecoin
		}

		// The macaroon files are passed to the wallet unlocker since they are
		// also encrypted with the wallet's password. These files will be
		// deleted within it and recreated when successfully changing the
		// wallet's password.
		macaroonFiles := []string{
			filepath.Join(networkDir, macaroons.DBFilename),
			cfg.AdminMacPath, cfg.ReadMacPath, cfg.InvoiceMacPath,
		}

		r.pwService = walletunlocker.New(
			chainConfig.ChainDir, activeNetParams.Params, !cfg.SyncFreelist,
			macaroonFiles,
		)
		lnrpc.RegisterWalletUnlockerServer(r.grpcServer, r.pwService)
	}

	// Initialize, and register our implementation of the gRPC interface
	// exported by the rpcServer.
	r.rpcServer, err = newRPCServer()
	if err != nil {
		err := fmt.Errorf("Unable to create RPC server: %v", err)
		ltndLog.Error(err)
		return err
	}

	lnrpc.RegisterLightningServer(r.grpcServer, r.rpcServer)

	registeredSubServers := lnrpc.RegisteredSubServers()
	for _, subServer := range registeredSubServers {
		subServerInstance, macPerms, err := subServer.New(deps.subServerCgs)
		if err != nil {
			return err
		}

		// We'll collect the sub-server, and also the set of
		// permissions it needs for macaroons so we can apply the
		// interceptors below.
		subServers = append(subServers, subServerInstance)
		subServerPerms = append(subServerPerms, macPerms)
	}

	// Now the main RPC server has been registered, we'll iterate through
	// all the sub-RPC servers and register them to ensure that requests
	// are properly routed towards them.
	for _, subServer := range r.rpcServer.subServers {
		err := subServer.RegisterWithRootServer(r.grpcServer)
		if err != nil {
			return fmt.Errorf("unable to register "+
				"sub-server %v with root: %v",
				subServer.Name(), err)
		}
	}

	return nil
}

func (r *rpcState) Serve(lisCfg ListenerCfg) error {

	// getListeners is a closure that creates listeners from the
	// RPCListeners defined in the config. It also returns a cleanup
	// closure and the server options to use for the GRPC server.
	var listeners []*ListenerWithSignal
	if lisCfg.RPCListener != nil {
		listeners = []*ListenerWithSignal{lisCfg.RPCListener}
	} else {
		for _, grpcEndpoint := range cfg.RPCListeners {
			// Start a gRPC server listening for HTTP/2
			// connections.
			lis, err := lncfg.ListenOnAddress(grpcEndpoint)
			if err != nil {
				ltndLog.Errorf("unable to listen on %s",
					grpcEndpoint)
				return err
			}
			listeners = append(
				listeners, &ListenerWithSignal{
					Listener: lis,
					Ready:    make(chan struct{}),
				})
		}
	}

	// TODO: listener cleanup

	// Use a WaitGroup so we can be sure the instructions on how to input the
	// password is the last thing to be printed to the console.
	var wg sync.WaitGroup

	// Get the listeners and server options to use for this rpc server.
	// With all the sub-servers started, we'll spin up the listeners for
	// the main RPC server itself.
	for _, lis := range listeners {
		wg.Add(1)
		go func(lis *ListenerWithSignal) {
			rpcsLog.Infof("RPC server listening on %s", lis.Addr())

			// Close the ready chan to indicate we are listening.
			close(lis.Ready)
			wg.Done()
			r.grpcServer.Serve(lis)
		}(lis)
	}

	// If Prometheus monitoring is enabled, start the Prometheus exporter.
	if cfg.Prometheus.Enabled() {
		err := monitoring.ExportPrometheusMetrics(
			r.grpcServer, cfg.Prometheus,
		)
		if err != nil {
			return err
		}
	}

	// The default JSON marshaler of the REST proxy only sets OrigName to
	// true, which instructs it to use the same field names as specified in
	// the proto file and not switch to camel case. What we also want is
	// that the marshaler prints all values, even if they are falsey.
	customMarshalerOption := proxy.WithMarshalerOption(
		proxy.MIMEWildcard, &proxy.JSONPb{
			OrigName:     true,
			EmitDefaults: true,
		},
	)

	// Finally, start the REST proxy for our gRPC server above. We'll ensure
	// we direct LND to connect to its loopback address rather than a
	// wildcard to prevent certificate issues when accessing the proxy
	// externally.
	//
	// TODO(roasbeef): eventually also allow the sub-servers to themselves
	// have a REST proxy.
	mux := proxy.NewServeMux(customMarshalerOption)

	// For our REST dial options, we'll still use TLS, but also increase
	// the max message size that we'll decode to allow clients to hit
	// endpoints which return more data such as the DescribeGraph call.
	// We set this to 200MiB atm. Should be the same value as maxMsgRecvSize
	// in cmd/lncli/main.go.
	restDialOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(*r.restCreds),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(1 * 1024 * 1024 * 200),
		),
	}

	err := lnrpc.RegisterLightningHandlerFromEndpoint(
		context.Background(), mux, r.restProxyDest,
		restDialOpts,
	)
	if err != nil {
		return err
	}

	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	err = lnrpc.RegisterWalletUnlockerHandlerFromEndpoint(
		ctx, mux, r.restProxyDest, restDialOpts,
	)
	if err != nil {
		return err
	}

	for _, restEndpoint := range cfg.RESTListeners {
		lis, err := lncfg.TLSListenOnAddress(restEndpoint, r.tlsCfg)
		if err != nil {
			ltndLog.Errorf(
				"gRPC proxy unable to listen on %s",
				restEndpoint,
			)
			return err
		}

		defer lis.Close()

		wg.Add(1)
		go func() {
			rpcsLog.Infof("gRPC proxy started at %s", lis.Addr())
			wg.Done()
			http.Serve(lis, mux)
		}()
	}

	// Wait for gRPC and REST servers to be up running.
	wg.Wait()

	return nil
}

func (r *rpcState) UnlockWallet() (*WalletUnlockParams, []byte, []byte, error) {
	var (
		walletInitParams WalletUnlockParams
		privateWalletPw  = lnwallet.DefaultPrivatePassphrase
		publicWalletPw   = lnwallet.DefaultPublicPassphrase
	)

	// If the user didn't request a seed, then we'll manually assume a
	// wallet birthday of now, as otherwise the seed would've specified
	// this information.
	walletInitParams.Birthday = time.Now()

	if !cfg.NoSeedBackup {
		params, err := waitForWalletPassword(r.pwService)
		if err != nil {
			err := fmt.Errorf("Unable to set up wallet password "+
				"listeners: %v", err)
			ltndLog.Error(err)
			return nil, nil, nil, err
		}

		walletInitParams = *params
		privateWalletPw = walletInitParams.Password
		publicWalletPw = walletInitParams.Password

		if walletInitParams.RecoveryWindow > 0 {
			ltndLog.Infof("Wallet recovery mode enabled with "+
				"address lookahead of %d addresses",
				walletInitParams.RecoveryWindow)
		}
	}

	if !cfg.NoMacaroons {
		// Try to unlock the macaroon store with the private password.
		err := r.macaroonService.CreateUnlock(&privateWalletPw)
		if err != nil {
			err := fmt.Errorf("Unable to unlock macaroons: %v", err)
			ltndLog.Error(err)
			return nil, nil, nil, err
		}

		// Create macaroon files for lncli to use if they don't exist.
		if !fileExists(cfg.AdminMacPath) && !fileExists(cfg.ReadMacPath) &&
			!fileExists(cfg.InvoiceMacPath) {

			ctx := context.Background()
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()

			err := genMacaroons(
				ctx, r.macaroonService, cfg.AdminMacPath,
				cfg.ReadMacPath, cfg.InvoiceMacPath,
			)
			if err != nil {
				err := fmt.Errorf("Unable to create macaroons "+
					"%v", err)
				ltndLog.Error(err)
				return nil, nil, nil, err
			}
		}
	}

	return &walletInitParams, privateWalletPw, publicWalletPw, nil
}

func (r *rpcState) StartLightningService(deps *rpcDeps) error {

	err := r.rpcServer.populateDependencies(
		deps,
	)
	if err != nil {
		err := fmt.Errorf("Unable to create RPC server: %v", err)
		ltndLog.Error(err)
		return err
	}
	if err := r.rpcServer.Start(); err != nil {
		err := fmt.Errorf("Unable to start RPC server: %v", err)
		ltndLog.Error(err)
		return err
	}
	defer r.rpcServer.Stop()

	return nil

}
