package server

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "net/http/pprof"

	"github.com/pkg/errors"
	"github.com/rexray/gocsi"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/CloudNativeAI/model-csi-driver/pkg/config"
	"github.com/CloudNativeAI/model-csi-driver/pkg/logger"
	"github.com/CloudNativeAI/model-csi-driver/pkg/metrics"
	"github.com/CloudNativeAI/model-csi-driver/pkg/provider"
	"github.com/CloudNativeAI/model-csi-driver/pkg/service"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
)

const authTokenKey = "authorization"

var kaep = keepalive.EnforcementPolicy{
	MinTime:             5 * time.Second, // If a client pings more than once every 5 seconds, terminate the connection
	PermitWithoutStream: true,            // Allow pings even when there are no active streams
}

var kasp = keepalive.ServerParameters{
	Time:    10 * time.Second, // Ping the client if it is idle for 10 seconds to ensure the connection is still active
	Timeout: 30 * time.Second, // Wait 30 second for the ping ack before assuming the connection is dead
}

func ensureSockNotExists(ctx context.Context, sockPath string) error {
	_, err := os.Stat(sockPath)
	if err == nil {
		if err = os.Remove(sockPath); err != nil {
			return errors.Wrapf(err, "remove existed sock path: %s", sockPath)
		}
		logger.WithContext(ctx).Infof("removed existed sock path: %s", sockPath)
	}
	if err = os.MkdirAll(filepath.Dir(sockPath), 0755); err != nil {
		return errors.Wrapf(err, "create sock path dir: %s", filepath.Dir(sockPath))
	}
	return nil
}

type Server struct {
	cfg *config.Config
	svc *service.Service
}

func NewServer(cfg *config.Config) (*Server, error) {
	svc, err := service.New(cfg)
	if err != nil {
		return nil, errors.Wrap(err, "create service")
	}
	return &Server{
		cfg: cfg,
		svc: svc,
	}, nil
}

func (server *Server) Service() *service.Service {
	return server.svc
}

func (server *Server) tokenAuthInterceptor(
	ctx context.Context,
	req interface{},
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (interface{}, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return nil, status.Errorf(codes.Unauthenticated, "Missing metadata")
	}

	tokens := md[authTokenKey]
	if server.cfg.ExternalCSIAuthorization != "" &&
		(len(tokens) == 0 || strings.TrimSpace(tokens[0]) != server.cfg.ExternalCSIAuthorization) {
		return nil, status.Errorf(codes.Unauthenticated, "Invalid token")
	}

	return handler(ctx, req)
}

func (server *Server) Run(ctx context.Context) error {
	eg, ctx := errgroup.WithContext(ctx)

	withFatalError := func(fn func() error) func() error {
		return func() error {
			err := fn()
			if err != nil {
				logger.WithContext(ctx).Fatal(err)
				os.Exit(1)
			}
			return err
		}
	}

	if server.cfg.PprofAddr != "" {
		eg.Go(withFatalError(func() error {
			endpoint, err := url.Parse(server.cfg.PprofAddr)
			if err != nil {
				return errors.Wrap(err, "parse pprof address")
			}

			lis, err := net.Listen(endpoint.Scheme, endpoint.Host)
			if err != nil {
				return errors.Wrap(err, "listen pprof server")
			}

			logger.WithContext(ctx).Infof("serving pprof server on %s", server.cfg.PprofAddr)

			return http.Serve(lis, nil)
		}))
	}

	eg.Go(withFatalError(func() error {
		endpoint, err := url.Parse(server.cfg.CSIEndpoint)
		if err != nil {
			return errors.Wrap(err, "parse external csi endpoint")
		}
		if endpoint.Path != "" {
			if err := ensureSockNotExists(ctx, endpoint.Path); err != nil {
				return errors.Wrapf(err, "ensure socket not exists: %s", endpoint.Path)
			}
		}

		os.Setenv("X_CSI_SPEC_VALIDATION", "false")
		os.Setenv("X_CSI_SPEC_REQ_VALIDATION", "false")
		os.Setenv("X_CSI_DEBUG", "false")
		os.Setenv("CSI_ENDPOINT", server.cfg.CSIEndpoint)

		pvd, err := provider.New(server.cfg, server.svc)
		if err != nil {
			return errors.Wrap(err, "create provider")
		}

		logger.WithContext(ctx).Infof("serving csi plugin on %s", server.cfg.CSIEndpoint)

		gocsi.Run(ctx, server.cfg.ServiceName, "A description of the SP", "", pvd)

		return nil
	}))

	if server.cfg.MetricsAddr != "" {
		eg.Go(withFatalError(func() error {
			metricsAddr := metrics.GetAddrByEnv(server.cfg.MetricsAddr, false)
			metricServer, err := metrics.NewServer(metricsAddr)
			if err != nil {
				return errors.Wrap(err, "create metrics server")
			}
			logger.WithContext(ctx).Infof("serving metrics server on %s", metricsAddr)
			go metricServer.Serve(ctx.Done())
			return nil
		}))

		if envPodIP := os.Getenv(metrics.EnvPodIP); envPodIP != "" {
			eg.Go(withFatalError(func() error {
				metricsAddr := metrics.GetAddrByEnv(server.cfg.MetricsAddr, true)
				metricServer, err := metrics.NewServer(metricsAddr)
				if err != nil {
					return errors.Wrap(err, "create metrics server")
				}
				logger.WithContext(ctx).Infof("serving metrics server on %s", metricsAddr)
				go metricServer.Serve(ctx.Done())
				return nil
			}))
		}
	}

	if server.cfg.IsNodeMode() {
		if server.cfg.ExternalCSIEndpoint != "" {
			eg.Go(withFatalError(func() error {
				endpoint, err := url.Parse(server.cfg.ExternalCSIEndpoint)
				if err != nil {
					return errors.Wrap(err, "parse external csi endpoint")
				}

				logger.WithContext(ctx).Infof("serving external grpc server on %s", server.cfg.ExternalCSIEndpoint)
				lis, err := net.Listen(endpoint.Scheme, endpoint.Host)
				if err != nil {
					return errors.Wrap(err, "listen external grpc server")
				}
				opts := []grpc.ServerOption{
					grpc.StatsHandler(otelgrpc.NewServerHandler()),
					grpc.KeepaliveEnforcementPolicy(kaep),
					grpc.KeepaliveParams(kasp),
					grpc.UnaryInterceptor(server.tokenAuthInterceptor),
				}
				grpcServer := grpc.NewServer(opts...)
				csi.RegisterControllerServer(grpcServer, server.svc)
				csi.RegisterIdentityServer(grpcServer, server.svc)
				csi.RegisterNodeServer(grpcServer, server.svc)
				return grpcServer.Serve(lis)
			}))
		}

		if server.cfg.DynamicCSIEndpoint != "" {
			eg.Go(withFatalError(func() error {
				endpoint, err := url.Parse(server.cfg.DynamicCSIEndpoint)
				if err != nil {
					return errors.Wrap(err, "parse dynamic csi endpoint")
				}
				if endpoint.Path != "" {
					if err := ensureSockNotExists(ctx, endpoint.Path); err != nil {
						return errors.Wrapf(err, "ensure socket not exists: %s", endpoint.Path)
					}
				}

				logger.WithContext(ctx).Infof("serving dynamic http server on %s", server.cfg.DynamicCSIEndpoint)

				httpServer, err := NewHTTPServer(server.cfg, server.svc)
				if err != nil {
					return errors.Wrap(err, "create dynamic http server")
				}

				return httpServer.Serve()
			}))
		}
	}

	if err := eg.Wait(); err != nil {
		metrics.NodeNotReady.Set(1)
	}

	return nil
}
