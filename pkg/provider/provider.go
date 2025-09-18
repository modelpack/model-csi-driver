package provider

import (
	"context"
	"net"

	"github.com/rexray/gocsi"
	log "github.com/sirupsen/logrus"

	"github.com/modelpack/model-csi-driver/pkg/config"
	"github.com/modelpack/model-csi-driver/pkg/service"
)

// New returns a new CSI Storage Plug-in Provider.
func New(cfg *config.Config, svc *service.Service) (gocsi.StoragePluginProvider, error) {
	return &gocsi.StoragePlugin{
		Controller: svc,
		Identity:   svc,
		Node:       svc,

		// BeforeServe allows the SP to participate in the startup
		// sequence. This function is invoked directly before the
		// gRPC server is created, giving the callback the ability to
		// modify the SP's interceptors, server options, or prevent the
		// server from starting by returning a non-nil error.
		BeforeServe: func(
			ctx context.Context,
			sp *gocsi.StoragePlugin,
			lis net.Listener) error {

			log.WithField("service", cfg.Get().ServiceName).Debug("BeforeServe")
			return nil
		},

		EnvVars: []string{
			// Enable request validation.
			gocsi.EnvVarSpecReqValidation + "=true",

			// Enable serial volume access.
			gocsi.EnvVarSerialVolAccess + "=true",
		},
	}, nil
}
