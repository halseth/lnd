package rpcperms

import (
	"context"
	"fmt"
	"sync"

	"github.com/btcsuite/btclog"
	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	"github.com/lightningnetwork/lnd/macaroons"
	"github.com/lightningnetwork/lnd/monitoring"
	"google.golang.org/grpc"
	"gopkg.in/macaroon-bakery.v2/bakery"
)

type RpcInterceptor struct {
	svc           *macaroons.Service
	permissionMap map[string][]bakery.Op
	rpcsLog       btclog.Logger

	sync.Mutex
}

func NewInterceptor(log btclog.Logger) *RpcInterceptor {
	return &RpcInterceptor{
		permissionMap: make(map[string][]bakery.Op),
		rpcsLog:       log,
	}
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

	// If macaroons aren't disabled (a non-nil service), then we'll set up
	// our set of interceptors which will allow us to handle the macaroon
	// authentication in a single location.
	macUnaryInterceptors := []grpc.UnaryServerInterceptor{}
	macStrmInterceptors := []grpc.StreamServerInterceptor{}

	unaryInterceptor := r.UnaryServerInterceptor()
	macUnaryInterceptors = append(macUnaryInterceptors, unaryInterceptor)

	strmInterceptor := r.StreamServerInterceptor()
	macStrmInterceptors = append(macStrmInterceptors, strmInterceptor)

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
