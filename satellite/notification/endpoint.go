// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package notification

import (
	"context"
	"net"

	"github.com/zeebo/errs"
	"go.uber.org/zap"

	"storj.io/storj/pkg/pb"
)

// Endpoint is the rpc handler for the notification system
type Endpoint struct {
	log     *zap.Logger
	service *Service
}

// drpcEndpoint wraps streaming methods so that they can be used with drpc
type drpcEndpoint struct{ *Endpoint }

// NewEndpoint creates a new notification endpoint.
func NewEndpoint(log *zap.Logger, service *Service) *Endpoint {
	return &Endpoint{
		log:     log,
		service: service,
	}
}

// DRPC returns a DRPC form of the endpoint.
func (endpoint *Endpoint) DRPC() pb.DRPCNotificationServer {
	return &drpcEndpoint{Endpoint: endpoint}
}

// ProcessNotification sends message to the specified set of nodes (ids)
func (endpoint *Endpoint) ProcessNotification(ctx context.Context, message *pb.NotificationMessage) (msg *pb.NotificationResponse, err error) {
	var eSent, rSent = false, false
	endpoint.log.Debug("sending to node", zap.String("address", message.Address), zap.String("message", string(message.Message)))
	if endpoint.service.CheckRPCLimit(message.NodeId.String()) {
		msg, err = endpoint.processNotificationRPC(ctx, message)
		if err != nil {
			return msg, err
		}
		rSent = true
	}
	if endpoint.service.CheckEmailLimit(message.NodeId.String()) {
		err = endpoint.processNotificationEmail(ctx, message)
		if err != nil {
			return msg, err
		}
		eSent = true
	}
	endpoint.service.IncrementLimiter(message.NodeId.String(), eSent, rSent)
	return msg, nil
}

func (endpoint *Endpoint) processNotificationRPC(ctx context.Context, message *pb.NotificationMessage) (_ *pb.NotificationResponse, err error) {
	client, err := newClient(ctx, endpoint.service.dialer, message.Address, message.NodeId)
	if err != nil {
		// if this is a network error, then return the error otherwise just report internal error
		_, ok := err.(net.Error)
		if ok {
			return &pb.NotificationResponse{}, Error.New("failed to connect to %s: %v", message.Address, err)
		}
		endpoint.log.Warn("internal error", zap.String("error", err.Error()))
		return &pb.NotificationResponse{}, Error.New("couldn't connect to client at addr: %s due to internal error.", message.Address)
	}
	defer func() { err = errs.Combine(err, client.Close()) }()

	return client.client.ProcessNotification(ctx, message)
}

func (endpoint *Endpoint) processNotificationEmail(ctx context.Context, message *pb.NotificationMessage) (err error) {
	//return endpoint.service.mailer.Send(ctx, &post.Message{})
	return nil
}

func (endpoint *Endpoint) sendBroadcastNotification(ctx context.Context, message string, ids []pb.Node) {
	var sentCount int
	var failed []string

	for _, node := range ids {
		// RPC Message
		mess := &pb.NotificationMessage{
			NodeId:   node.Id,
			Address:  node.Address.Address,
			Loglevel: pb.LogLevel_INFO,
			Message:  []byte(message),
		}

		_, err := endpoint.ProcessNotification(ctx, mess)
		if err != nil {
			failed = append(failed, node.Id.String())
		}
		sentCount++
	}

	endpoint.log.Info("sent to nodes", zap.Int("count", sentCount))
	endpoint.log.Debug("notification to the following nodes failed", zap.Strings("nodeIDs", failed))
}
