package service

import (
	"context"

	"github.com/modelpack/modctl/pkg/backend"
	"github.com/modelpack/model-csi-driver/pkg/config/auth"
	"github.com/pkg/errors"
)

func (s *Service) GetArtifact(ctx context.Context, reference string) (*backend.InspectedModelArtifact, error) {
	keyChain, err := auth.GetKeyChainByRef(reference)
	if err != nil {
		return nil, errors.Wrapf(err, "get auth for model: %s", reference)
	}
	plainHTTP := keyChain.ServerScheme == "http"

	b, err := backend.New("")
	if err != nil {
		return nil, errors.Wrap(err, "create modctl backend")
	}

	modelArtifact := NewModelArtifact(b, reference, plainHTTP)

	artifact, err := modelArtifact.Inspect(ctx, reference)
	if err != nil {
		return nil, err
	}

	return artifact, nil
}
