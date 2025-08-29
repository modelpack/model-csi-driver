package server

import (
	"net"
	"net/http"
	"net/url"

	"github.com/CloudNativeAI/model-csi-driver/pkg/config"
	"github.com/CloudNativeAI/model-csi-driver/pkg/service"
	"github.com/labstack/echo/v4"
	"github.com/pkg/errors"
)

const (
	ERR_CODE_INVALID_ARGUMENT        = "INVALID_ARGUMENT"
	ERR_CODE_INTERNAL                = "INTERNAL"
	ERR_CODE_NOT_FOUND               = "NOT_FOUND"
	ERR_CODE_INSUFFICIENT_DISK_QUOTA = "INSUFFICIENT_DISK_QUOTA"
)

type HttpServer struct {
	cfg      *config.Config
	echo     *echo.Echo
	svc      *service.Service
	server   *http.Server
	listener net.Listener
}

type ErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func NewHTTPServer(cfg *config.Config, svc *service.Service) (*HttpServer, error) {
	echo := echo.New()

	endpoint, err := url.Parse(cfg.DynamicCSIEndpoint)
	if err != nil {
		return nil, errors.Wrapf(err, "parse dynamic csi endpoint: %s", cfg.DynamicCSIEndpoint)
	}

	listener, err := net.Listen("unix", endpoint.Path)
	if err != nil {
		return nil, errors.Wrapf(err, "listen dynamic csi sock: %s", endpoint.Path)
	}

	return &HttpServer{
		echo: echo,
		cfg:  cfg,
		svc:  svc,
		server: &http.Server{
			Handler: echo,
		},
		listener: listener,
	}, nil
}

func (s *HttpServer) Serve() error {
	handler := &HttpHandler{
		cfg: s.cfg,
		svc: s.svc,
	}

	s.echo.POST("/api/v1/volumes/:volume_name/mounts", handler.CreateVolume)
	s.echo.GET("/api/v1/volumes/:volume_name/mounts/:mount_id", handler.GetVolume)
	s.echo.DELETE("/api/v1/volumes/:volume_name/mounts/:mount_id", handler.DeleteVolume)
	s.echo.GET("/api/v1/volumes/:volume_name/mounts", handler.ListVolumes)

	if err := s.server.Serve(s.listener); err != nil && err != http.ErrServerClosed {
		return errors.Wrap(err, "serve http server")
	}

	return nil
}
