// Code generated by falafel 0.7. DO NOT EDIT.
// source: wtclientrpc/wtclient.proto

// +build wtclientrpc

package lndmobile

import (
	"context"

	"github.com/golang/protobuf/proto"

	"github.com/lightningnetwork/lnd/lnrpc/wtclientrpc"
)

// getWatchtowerClientClient returns a client connection to the server listening
// on lis.
func getWatchtowerClientClient() (wtclientrpc.WatchtowerClientClient, func(), error) {
	clientConn, closeConn, err := getLightningLisConn()
	if err != nil {
		return nil, nil, err
	}
	client := wtclientrpc.NewWatchtowerClientClient(clientConn)
	return client, closeConn, nil
}

// AddTower adds a new watchtower reachable at the given address and
// considers it for new sessions. If the watchtower already exists, then
// any new addresses included will be considered when dialing it for
// session negotiations and backups.
//
// NOTE: This method produces a single result or error, and the callback will
// be called only once.
func AddTower(msg []byte, callback Callback) {
	s := &syncHandler{
		newProto: func() proto.Message {
			return &wtclientrpc.AddTowerRequest{}
		},
		getSync: func(ctx context.Context,
			req proto.Message) (proto.Message, error) {

			// Get the gRPC client.
			client, closeClient, err := getWatchtowerClientClient()
			if err != nil {
				return nil, err
			}
			defer closeClient()

			r := req.(*wtclientrpc.AddTowerRequest)
			return client.AddTower(ctx, r)
		},
	}
	s.start(msg, callback)
}

// RemoveTower removes a watchtower from being considered for future session
// negotiations and from being used for any subsequent backups until it's added
// again. If an address is provided, then this RPC only serves as a way of
// removing the address from the watchtower instead.
//
// NOTE: This method produces a single result or error, and the callback will
// be called only once.
func RemoveTower(msg []byte, callback Callback) {
	s := &syncHandler{
		newProto: func() proto.Message {
			return &wtclientrpc.RemoveTowerRequest{}
		},
		getSync: func(ctx context.Context,
			req proto.Message) (proto.Message, error) {

			// Get the gRPC client.
			client, closeClient, err := getWatchtowerClientClient()
			if err != nil {
				return nil, err
			}
			defer closeClient()

			r := req.(*wtclientrpc.RemoveTowerRequest)
			return client.RemoveTower(ctx, r)
		},
	}
	s.start(msg, callback)
}

//
// NOTE: This method produces a single result or error, and the callback will
// be called only once.
func ListTowers(msg []byte, callback Callback) {
	s := &syncHandler{
		newProto: func() proto.Message {
			return &wtclientrpc.ListTowersRequest{}
		},
		getSync: func(ctx context.Context,
			req proto.Message) (proto.Message, error) {

			// Get the gRPC client.
			client, closeClient, err := getWatchtowerClientClient()
			if err != nil {
				return nil, err
			}
			defer closeClient()

			r := req.(*wtclientrpc.ListTowersRequest)
			return client.ListTowers(ctx, r)
		},
	}
	s.start(msg, callback)
}

//
// NOTE: This method produces a single result or error, and the callback will
// be called only once.
func GetTowerInfo(msg []byte, callback Callback) {
	s := &syncHandler{
		newProto: func() proto.Message {
			return &wtclientrpc.GetTowerInfoRequest{}
		},
		getSync: func(ctx context.Context,
			req proto.Message) (proto.Message, error) {

			// Get the gRPC client.
			client, closeClient, err := getWatchtowerClientClient()
			if err != nil {
				return nil, err
			}
			defer closeClient()

			r := req.(*wtclientrpc.GetTowerInfoRequest)
			return client.GetTowerInfo(ctx, r)
		},
	}
	s.start(msg, callback)
}

//
// NOTE: This method produces a single result or error, and the callback will
// be called only once.
func Stats(msg []byte, callback Callback) {
	s := &syncHandler{
		newProto: func() proto.Message {
			return &wtclientrpc.StatsRequest{}
		},
		getSync: func(ctx context.Context,
			req proto.Message) (proto.Message, error) {

			// Get the gRPC client.
			client, closeClient, err := getWatchtowerClientClient()
			if err != nil {
				return nil, err
			}
			defer closeClient()

			r := req.(*wtclientrpc.StatsRequest)
			return client.Stats(ctx, r)
		},
	}
	s.start(msg, callback)
}

//
// NOTE: This method produces a single result or error, and the callback will
// be called only once.
func Policy(msg []byte, callback Callback) {
	s := &syncHandler{
		newProto: func() proto.Message {
			return &wtclientrpc.PolicyRequest{}
		},
		getSync: func(ctx context.Context,
			req proto.Message) (proto.Message, error) {

			// Get the gRPC client.
			client, closeClient, err := getWatchtowerClientClient()
			if err != nil {
				return nil, err
			}
			defer closeClient()

			r := req.(*wtclientrpc.PolicyRequest)
			return client.Policy(ctx, r)
		},
	}
	s.start(msg, callback)
}
