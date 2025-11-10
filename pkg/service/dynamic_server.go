package service

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/labstack/echo/v4"
	"github.com/modelpack/model-csi-driver/pkg/config"
	"github.com/modelpack/model-csi-driver/pkg/logger"
	"github.com/modelpack/model-csi-driver/pkg/utils"
	"github.com/pkg/errors"
)

const (
	ERR_CODE_INVALID_ARGUMENT        = "INVALID_ARGUMENT"
	ERR_CODE_INTERNAL                = "INTERNAL"
	ERR_CODE_NOT_FOUND               = "NOT_FOUND"
	ERR_CODE_INSUFFICIENT_DISK_QUOTA = "INSUFFICIENT_DISK_QUOTA"
)

type DynamicServer struct {
	cfg      *config.Config
	echo     *echo.Echo
	svc      *Service
	server   *http.Server
	listener net.Listener
}

type ErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type DynamicServerManager struct {
	cfg *config.Config
	svc *Service

	mutex   sync.Mutex
	servers map[string]*DynamicServer
}

func NewDynamicServerManager(cfg *config.Config, svc *Service) *DynamicServerManager {
	return &DynamicServerManager{
		cfg:     cfg,
		svc:     svc,
		servers: make(map[string]*DynamicServer),
	}
}

func (m *DynamicServerManager) CreateServer(ctx context.Context, sockPath string) (*DynamicServer, error) {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if server, exists := m.servers[sockPath]; exists {
		_ = server.server.Close()
		_ = server.listener.Close()
		delete(m.servers, sockPath)
	}

	server, err := newDynamicServer(ctx, m.cfg, m.svc, sockPath)
	if err != nil {
		return nil, errors.Wrapf(err, "create http server on sock: %s", sockPath)
	}

	go func() {
		if err := server.serve(); err != nil {
			logger.WithContext(ctx).WithError(err).Warnf("http server unexpected closed: %s", sockPath)
			return
		}
		logger.WithContext(ctx).Infof("http server closed: %s", sockPath)
	}()

	m.servers[sockPath] = server

	logger.WithContext(ctx).Infof("created dynamic server on %s", sockPath)

	return server, nil
}

func (m *DynamicServerManager) CloseServer(ctx context.Context, sockPath string) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	server, exists := m.servers[sockPath]
	if !exists {
		return nil
	}

	if err := server.server.Close(); err != nil {
		logger.WithContext(ctx).WithError(err).Warnf("close http server on sock: %s", sockPath)
	}
	if err := server.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		logger.WithContext(ctx).WithError(err).Warnf("close listener on sock: %s", sockPath)
	}

	delete(m.servers, sockPath)

	logger.WithContext(ctx).Infof("closed dynamic server on %s", sockPath)

	return nil
}

func (m *DynamicServerManager) RecoverServers(ctx context.Context) error {
	volumesDir := m.cfg.Get().GetVolumesDir()
	volumeDirs, err := os.ReadDir(volumesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return errors.Wrapf(err, "read volume dirs from %s", volumesDir)
	}

	for _, volumeDir := range volumeDirs {
		volumeName := volumeDir.Name()
		csiSockDir := m.cfg.Get().GetCSISockDirForDynamic(volumeName)
		csiSockDirStat, err := os.Stat(csiSockDir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return errors.Wrapf(err, "stat dynamic csi sock dir: %s", csiSockDir)
		}
		if !csiSockDirStat.IsDir() {
			continue
		}
		volumeDir := m.cfg.Get().GetVolumeDirForDynamic(volumeName)
		sameDevice, err := utils.IsInSameDevice(volumeDir, csiSockDir)
		if err != nil {
			return errors.Wrapf(err, "check same device for volume dir: %s", volumeDir)
		}
		if !sameDevice {
			// Deprecated: use DynamicServerManager to manage dynamic csi.sock servers,
			// keep this for backward compatibility.
			logger.WithContext(ctx).Infof("skip recover dynamic csi server on different device: %s", csiSockDir)
			continue
		}
		if _, err := m.CreateServer(ctx, m.cfg.Get().GetCSISockPathForDynamic(volumeName)); err != nil {
			logger.WithContext(ctx).WithError(err).Errorf("recover dynamic csi server on: %s", csiSockDir)
		} else {
			logger.WithContext(ctx).Infof("recovered dynamic csi server on: %s", csiSockDir)
		}
	}

	return nil
}

func newDynamicServer(
	ctx context.Context, cfg *config.Config, svc *Service, sockPath string,
) (*DynamicServer, error) {
	if err := utils.EnsureSockNotExists(ctx, sockPath); err != nil {
		return nil, errors.Wrapf(err, "ensure socket not exists: %s", sockPath)
	}

	// HACK: Temporarily change workdir to the sockPath directory to prevent
	// socket path from being too long (108 bytes limitation by kernel),
	// which could cause listening to fail: "bind: invalid argument".
	origDir, err := os.Getwd()
	if err != nil {
		return nil, errors.Wrap(err, "getwd before chdir")
	}
	defer func() {
		_ = os.Chdir(origDir)
	}()
	if err := os.Chdir(filepath.Dir(sockPath)); err != nil {
		return nil, errors.Wrapf(err, "chdir to sock dir: %s", filepath.Dir(sockPath))
	}

	listener, err := net.Listen("unix", filepath.Base(sockPath))
	if err != nil {
		return nil, errors.Wrapf(err, "listen dynamic csi sock: %s", sockPath)
	}

	echo := echo.New()

	return &DynamicServer{
		echo: echo,
		cfg:  cfg,
		svc:  svc,
		server: &http.Server{
			Handler: echo,
		},
		listener: listener,
	}, nil
}

func (s *DynamicServer) serve() error {
	handler := &DynamicServerHandler{
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
