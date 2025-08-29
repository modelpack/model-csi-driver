package client

import (
	"context"
	"strings"
	"time"

	"github.com/CloudNativeAI/model-csi-driver/pkg/config"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/pkg/errors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"google.golang.org/grpc/keepalive"
)

const authTokenKey = "authorization"

var kacp = keepalive.ClientParameters{
	Time:                30 * time.Second, // send pings every 30 seconds if there is no activity
	Timeout:             10 * time.Second, // wait 10 second for ping ack before considering the connection dead
	PermitWithoutStream: true,             // send pings even without active streams
}

type GRPCClient struct {
	cfg  *config.Config
	conn *grpc.ClientConn
}

func NewGRPCClient(cfg *config.Config, addr string) (*GRPCClient, error) {
	addr = strings.TrimPrefix(addr, "tcp://")

	conn, err := grpc.NewClient(
		addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(kacp),
		grpc.WithUnaryInterceptor(func(
			ctx context.Context,
			method string,
			req, reply interface{},
			cc *grpc.ClientConn,
			invoker grpc.UnaryInvoker,
			opts ...grpc.CallOption,
		) error {
			newCtx := metadata.AppendToOutgoingContext(ctx, authTokenKey, cfg.ExternalCSIAuthorization)
			return invoker(newCtx, method, req, reply, cc, opts...)
		}),
	)
	if err != nil {
		return nil, errors.Wrapf(err, "connect to grpc server: %s", addr)
	}

	return &GRPCClient{
		cfg:  cfg,
		conn: conn,
	}, nil
}

func (c *GRPCClient) Close() error {
	if c.conn != nil {
		if err := c.conn.Close(); err != nil {
			return errors.Wrap(err, "close grpc connection")
		}
	}
	return nil
}

func (c *GRPCClient) CreateVolume(ctx context.Context, volumeName string, parameters map[string]string) (*csi.CreateVolumeResponse, error) {
	client := csi.NewControllerClient(c.conn)
	resp, err := client.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:       volumeName,
		Parameters: parameters,
	})
	if err != nil {
		return nil, errors.Wrapf(err, "create volume")
	}
	return resp, nil
}

func (c *GRPCClient) DeleteVolume(ctx context.Context, volumeID string) (*csi.DeleteVolumeResponse, error) {
	client := csi.NewControllerClient(c.conn)
	resp, err := client.DeleteVolume(ctx, &csi.DeleteVolumeRequest{
		VolumeId: volumeID,
	})
	if err != nil {
		return nil, errors.Wrapf(err, "delete volume")
	}
	return resp, nil
}

func (c *GRPCClient) PublishVolume(ctx context.Context, volumeID, targetPath string) (*csi.NodePublishVolumeResponse, error) {
	client := csi.NewNodeClient(c.conn)
	resp, err := client.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
		VolumeId:   volumeID,
		TargetPath: targetPath,
	})
	if err != nil {
		return nil, errors.Wrapf(err, "publish volume")
	}
	return resp, nil
}

func (c *GRPCClient) UnpublishVolume(ctx context.Context, volumeID, targetPath string) (*csi.NodeUnpublishVolumeResponse, error) {
	client := csi.NewNodeClient(c.conn)
	resp, err := client.NodeUnpublishVolume(ctx, &csi.NodeUnpublishVolumeRequest{
		VolumeId:   volumeID,
		TargetPath: targetPath,
	})
	if err != nil {
		return nil, errors.Wrapf(err, "unpublish volume")
	}
	return resp, nil
}

func (c *GRPCClient) PublishStaticInlineVolume(ctx context.Context, volumeID, targetPath, reference string) (*csi.NodePublishVolumeResponse, error) {
	client := csi.NewNodeClient(c.conn)
	resp, err := client.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{
		VolumeId:   volumeID,
		TargetPath: targetPath,
		VolumeContext: map[string]string{
			c.cfg.ParameterKeyReference(): reference,
		},
	})
	if err != nil {
		return nil, errors.Wrapf(err, "publish volume")
	}
	return resp, nil
}
