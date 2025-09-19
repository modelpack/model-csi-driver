package service

import (
	"net/url"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/modelpack/model-csi-driver/pkg/config"
	"github.com/modelpack/model-csi-driver/pkg/status"
	"github.com/modelpack/model-csi-driver/pkg/tracing"
	"github.com/pkg/errors"
	v1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

const (
	// VendorVersion is the version of this CSP SP.
	VendorVersion = "0.1.0"

	labelHostname          = "kubernetes.io/hostname"
	annotationSelectedNode = "volume.kubernetes.io/selected-node"
)

type Service struct {
	csi.UnimplementedControllerServer
	csi.UnimplementedIdentityServer
	csi.UnimplementedNodeServer

	cfg *config.Config

	// only for node mode
	dynamicCSISockPath string
	sm                 *status.StatusManager
	cm                 *CacheManager
	worker             *Worker

	// only for controller mode
	remoteGRPCPort string
	node           v1.NodeInterface
}

func (svc *Service) StatusManager() *status.StatusManager {
	return svc.sm
}

func New(cfg *config.Config) (*Service, error) {
	if err := tracing.Init(cfg); err != nil {
		return nil, errors.Wrap(err, "initialize tracing")
	}

	svc := Service{
		cfg: cfg,
	}

	if cfg.Get().IsControllerMode() {
		externalCSIEndpoint := cfg.Get().ExternalCSIEndpoint
		url, err := url.Parse(externalCSIEndpoint)
		if err != nil {
			return nil, errors.Wrapf(err, "parse external csi endpoint: %s", externalCSIEndpoint)
		}
		if url.Port() == "" {
			return nil, errors.Errorf("external csi endpoint: %s must have a port", externalCSIEndpoint)
		}
		clientset, err := loadKubeConfig()
		if err != nil {
			return nil, errors.Wrap(err, "load kube config")
		}
		svc.remoteGRPCPort = url.Port()
		svc.node = clientset.CoreV1().Nodes()
	} else {
		sm, err := status.NewStatusManager()
		if err != nil {
			return nil, errors.Wrap(err, "create status manager")
		}
		worker, err := NewWorker(cfg, sm)
		if err != nil {
			return nil, errors.Wrap(err, "create worker")
		}
		cm, err := NewCacheManager(cfg)
		if err != nil {
			return nil, errors.Wrap(err, "create cache manager")
		}
		if cfg.Get().DynamicCSIEndpoint != "" {
			endpoint, err := url.Parse(cfg.Get().DynamicCSIEndpoint)
			if err != nil {
				return nil, errors.Wrap(err, "parse dynamic csi endpoint")
			}
			if endpoint.Path == "" {
				return nil, errors.Errorf("dynamic csi endpoint: %s must have a path", cfg.Get().DynamicCSIEndpoint)
			}
			svc.dynamicCSISockPath = endpoint.Path
		}
		svc.sm = sm
		svc.cm = cm
		svc.worker = worker
	}

	return &svc, nil
}
