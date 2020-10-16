package rpcperms

import (
	"context"
	"fmt"
	"sync"

	"github.com/btcsuite/btclog"
	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	"github.com/lightningnetwork/lnd/lnrpc"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/lightningnetwork/lnd/monitoring"
	"google.golang.org/grpc"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

type rpcState uint8

const (
	inactive rpcState = iota
	walletLocked
	walletUnlocked
	rpcActive
)

type RpcInterceptor struct {
	state rpcState

	svc           *macaroons.Service
	permissionMap map[string][]bakery.Op
	rpcsLog       btclog.Logger

	sync.Mutex
}

func NewInterceptor(log btclog.Logger) *RpcInterceptor {
	return &RpcInterceptor{
		state:         inactive,
		permissionMap: make(map[string][]bakery.Op),
		rpcsLog:       log,
	}
}

func (r *RpcInterceptor) SetWalletLocked() {
	r.state = walletLocked
}

func (r *RpcInterceptor) SetWalletUnlocked() {
	r.state = walletUnlocked
}

func (r *RpcInterceptor) SetRpcActive() {
	r.state = rpcActive
}

func (r *RpcInterceptor) AddMacaroonService(svc *macaroons.Service) {
	r.Lock()
	defer r.Unlock()

	r.svc = svc
}

func (r *RpcInterceptor) AddPermission(method string, ops []bakery.Op) error {
	r.Lock()
	defer r.Unlock()

	if _, ok := r.permissionMap[method]; ok {
		return fmt.Errorf("detected "+
			"duplicate macaroon "+
			"constraints for path: %v",
			method)
	}

	r.permissionMap[method] = ops
	return nil
}

func (r *RpcInterceptor) Permissions() map[string][]bakery.Op {
	//TODO: mutex?
	return r.permissionMap
}

func (r *RpcInterceptor) CreateServerOpts() []grpc.ServerOption {

	var unaryInterceptors []grpc.UnaryServerInterceptor
	var strmInterceptors []grpc.StreamServerInterceptor

	unaryInterceptors = append(
		unaryInterceptors, r.rpcStateUnaryServerInterceptor(),
	)
	strmInterceptors = append(
		strmInterceptors, r.rpcStateStreamServerInterceptor(),
	)

	// If macaroons aren't disabled (a non-nil service), then we'll set up
	// our set of interceptors which will allow us to handle the macaroon
	// authentication in a single location.
	unaryInterceptors = append(
		unaryInterceptors, r.UnaryServerInterceptor(),
	)
	strmInterceptors = append(
		strmInterceptors, r.StreamServerInterceptor(),
	)

	// Get interceptors for Prometheus to gather gRPC performance metrics.
	// If monitoring is disabled, GetPromInterceptors() will return empty
	// slices.
	promUnaryInterceptors, promStrmInterceptors := monitoring.GetPromInterceptors()

	// Concatenate the slices of unary and stream interceptors respectively.
	unaryInterceptors = append(unaryInterceptors, promUnaryInterceptors...)
	strmInterceptors = append(strmInterceptors, promStrmInterceptors...)

	// We'll also add our logging interceptors as well, so we can
	// automatically log all errors that happen during RPC calls.
	unaryInterceptors = append(
		unaryInterceptors, errorLogUnaryServerInterceptor(r.rpcsLog),
	)
	strmInterceptors = append(
		strmInterceptors, errorLogStreamServerInterceptor(r.rpcsLog),
	)
	var serverOpts []grpc.ServerOption

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

	return serverOpts
}

// errorLogUnaryServerInterceptor is a simple UnaryServerInterceptor that will
// automatically log any errors that occur when serving a client's unary
// request.
func errorLogUnaryServerInterceptor(logger btclog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (interface{}, error) {

		resp, err := handler(ctx, req)
		if err != nil {
			// TODO(roasbeef): also log request details?
			logger.Errorf("[%v]: %v", info.FullMethod, err)
		}

		return resp, err
	}
}

// errorLogStreamServerInterceptor is a simple StreamServerInterceptor that
// will log any errors that occur while processing a client or server streaming
// RPC.
func errorLogStreamServerInterceptor(logger btclog.Logger) grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream,
		info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {

		err := handler(srv, ss)
		if err != nil {
			logger.Errorf("[%v]: %v", info.FullMethod, err)
		}

		return err
	}
}

// UnaryServerInterceptor is a GRPC interceptor that checks whether the
// request is authorized by the included macaroons.
func (r *RpcInterceptor) UnaryServerInterceptor() grpc.UnaryServerInterceptor {

	return func(ctx context.Context, req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (interface{}, error) {

		if r.svc == nil {
			return handler(ctx, req)
		}

		svc := r.svc

		uriPermissions, ok := r.permissionMap[info.FullMethod]
		if !ok {
			return nil, fmt.Errorf("%s: unknown permissions "+
				"required for method", info.FullMethod)
		}

		// Find out if there is an external validator registered for
		// this method. Fall back to the internal one if there isn't.
		validator, ok := svc.ExternalValidators[info.FullMethod]
		if !ok {
			validator = svc
		}

		// Now that we know what validator to use, let it do its work.
		err := validator.ValidateMacaroon(
			ctx, uriPermissions, info.FullMethod,
		)
		if err != nil {
			return nil, err
		}

		return handler(ctx, req)
	}
}

// StreamServerInterceptor is a GRPC interceptor that checks whether the
// request is authorized by the included macaroons.
func (r *RpcInterceptor) StreamServerInterceptor() grpc.StreamServerInterceptor {

	return func(srv interface{}, ss grpc.ServerStream,
		info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if r.svc == nil {
			return handler(srv, ss)
		}
		svc := r.svc

		uriPermissions, ok := r.permissionMap[info.FullMethod]
		if !ok {
			return fmt.Errorf("%s: unknown permissions required "+
				"for method", info.FullMethod)
		}

		// Find out if there is an external validator registered for
		// this method. Fall back to the internal one if there isn't.
		validator, ok := svc.ExternalValidators[info.FullMethod]
		if !ok {
			validator = svc
		}

		// Now that we know what validator to use, let it do its work.
		err := validator.ValidateMacaroon(
			ss.Context(), uriPermissions, info.FullMethod,
		)
		if err != nil {
			return err
		}

		return handler(srv, ss)
	}
}

func (r *RpcInterceptor) checkRpcState(srv interface{}) error {
	switch r.state {
	case inactive:
		return fmt.Errorf("rpc not active")
	case walletLocked:
		return fmt.Errorf("wallet locked")

	case rpcActive:
		_, ok1 := srv.(lnrpc.LightningServer)
		_, ok2 := srv.(lnrpc.SubServer)
		if !ok1 && !ok2 {
			return fmt.Errorf("invalid service")
		}

	default:
		return fmt.Errorf("unknown rpc state: %v", r.state)
	}

	return nil
}

func (r *RpcInterceptor) rpcStateUnaryServerInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler) (interface{}, error) {

		if err := r.checkRpcState(info.Server); err != nil {
			return nil, err
		}

		return handler(ctx, req)
	}
}

func (r *RpcInterceptor) rpcStateStreamServerInterceptor() grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream,
		info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {

		if err := r.checkRpcState(srv); err != nil {
			return err
		}

		return handler(srv, ss)
	}
}
