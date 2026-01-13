package service

import (
	"fmt"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel/attribute"
	otelCodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/modelpack/model-csi-driver/pkg/logger"
	"github.com/modelpack/model-csi-driver/pkg/tracing"
	"github.com/pkg/errors"
)

const authTokenKey = "authorization"

var kacp = keepalive.ClientParameters{
	Time:                30 * time.Second, // send pings every 30 seconds if there is no activity
	Timeout:             10 * time.Second, // wait 10 second for ping ack before considering the connection dead
	PermitWithoutStream: true,             // send pings even without active streams
}

func (s *Service) tokenAuthInterceptor(
	ctx context.Context,
	method string,
	req, reply interface{},
	cc *grpc.ClientConn,
	invoker grpc.UnaryInvoker,
	opts ...grpc.CallOption,
) error {
	newCtx := metadata.AppendToOutgoingContext(ctx, authTokenKey, s.cfg.Get().ExternalCSIAuthorization)
	return invoker(newCtx, method, req, reply, cc, opts...)
}

func (s *Service) getNodeInfoByName(ctx context.Context, nodeName string) (*nodeInfo, error) {
	node, err := s.getNode(ctx, nodeName)
	if err != nil {
		return nil, errors.Wrapf(err, "get node by name: %s", nodeName)
	}

	nodeInfo, err := getNodeInfo(node)
	if err != nil {
		return nil, errors.Wrapf(err, "get node info by name: %s", nodeName)
	}

	return nodeInfo, nil
}

func (s *Service) remoteCreateVolume(
	ctx context.Context,
	req *csi.CreateVolumeRequest) (
	*csi.CreateVolumeResponse, error) {
	parameters := req.GetParameters()
	if parameters == nil {
		parameters = map[string]string{}
	}

	nodeName := parameters[annotationSelectedNode]
	if nodeName == "" {
		return nil, status.Errorf(codes.InvalidArgument, "empty annotation %s in PVC", annotationSelectedNode)
	}

	parentSpan := trace.SpanFromContext(ctx)
	parentSpan.SetAttributes(attribute.String("node_name", nodeName))

	_, span := tracing.Tracer.Start(ctx, "GetNodeInfoByName")
	nodeInfo, err := s.getNodeInfoByName(ctx, nodeName)
	if err != nil {
		span.SetStatus(otelCodes.Error, "failed to get node info")
		span.RecordError(err)
		span.End()
		return nil, errors.Wrapf(err, "get node IP by name: %s", nodeName)
	}
	span.End()

	volumeName := req.GetName()
	parameters[s.cfg.Get().ParameterVolumeContextNodeIP()] = nodeInfo.ip

	parentSpan.SetAttributes(attribute.String("volume_name", volumeName))
	parentSpan.SetAttributes(attribute.String("node_ip", nodeInfo.ip))
	parentSpan.SetAttributes(attribute.String("node_hostname", nodeInfo.hostname))

	addr := fmt.Sprintf("%s:%s", nodeInfo.ip, s.remoteGRPCPort)
	logger.WithContext(ctx).Infof("calling remote grpc: %s", addr)

	conn, err := grpc.NewClient(
		addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
		grpc.WithKeepaliveParams(kacp),
		grpc.WithUnaryInterceptor(s.tokenAuthInterceptor),
	)
	if err != nil {
		return nil, errors.Wrapf(err, "connect to grpc server: %s", addr)
	}
	defer func() { _ = conn.Close() }()

	client := csi.NewControllerClient(conn)
	resp, err := client.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:       volumeName,
		Parameters: parameters,
	})
	if err != nil {
		return nil, errors.Wrapf(err, "call grpc server: %s", addr)
	}

	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      resp.GetVolume().GetVolumeId(),
			CapacityBytes: req.GetCapacityRange().GetRequiredBytes(),
			VolumeContext: parameters,
			AccessibleTopology: []*csi.Topology{
				{
					Segments: map[string]string{
						labelHostname: nodeInfo.hostname,
					},
				},
			},
		},
	}, nil
}

func (s *Service) remoteDeleteVolume(
	ctx context.Context,
	req *csi.DeleteVolumeRequest) (
	*csi.DeleteVolumeResponse, error) {
	parameters := req.GetSecrets()
	if parameters == nil {
		parameters = map[string]string{}
	}

	nodeName := parameters[annotationSelectedNode]
	if nodeName == "" {
		return nil, status.Errorf(codes.InvalidArgument, "empty annotation %s in PVC", annotationSelectedNode)
	}
	_, span := tracing.Tracer.Start(ctx, "GetNodeInfoByName")
	span.SetAttributes(attribute.String("node_name", nodeName))
	nodeInfo, err := s.getNodeInfoByName(ctx, nodeName)
	if err != nil {
		span.SetStatus(otelCodes.Error, "failed to get node info")
		span.RecordError(err)
		span.End()
		// If node not found, we just return success to avoid orphaned volume.
		if apierrors.IsNotFound(err) {
			logger.WithContext(ctx).WithError(err).Warnf("node %s not found, return success for deleting volume", nodeName)
			return &csi.DeleteVolumeResponse{}, nil
		}
		return nil, errors.Wrapf(err, "get node IP by name: %s", nodeName)
	}
	span.End()
	nodeIP := nodeInfo.ip

	volumeID := req.GetVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "empty volumeId")
	}

	parentSpan := trace.SpanFromContext(ctx)
	parentSpan.SetAttributes(attribute.String("volume_name", volumeID))
	parentSpan.SetAttributes(attribute.String("node_ip", nodeIP))

	addr := fmt.Sprintf("%s:%s", nodeIP, s.remoteGRPCPort)
	logger.WithContext(ctx).Infof("calling remote grpc: %s", addr)

	conn, err := grpc.NewClient(
		addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
		grpc.WithKeepaliveParams(kacp),
		grpc.WithUnaryInterceptor(s.tokenAuthInterceptor),
	)
	if err != nil {
		return nil, errors.Wrapf(err, "connect to grpc server: %s", addr)
	}
	defer func() { _ = conn.Close() }()

	client := csi.NewControllerClient(conn)
	resp, err := client.DeleteVolume(ctx, &csi.DeleteVolumeRequest{
		VolumeId: volumeID,
	})
	if err != nil {
		return nil, errors.Wrapf(err, "call grpc server: %s", addr)
	}

	return resp, nil
}

func (s *Service) remoteListVolumes(
	ctx context.Context,
	req *csi.ListVolumesRequest) (
	*csi.ListVolumesResponse, error) {
	return nil, status.Error(codes.Unimplemented, "remote list volumes not implemented yet")
}
