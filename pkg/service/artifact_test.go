package service

import (
	"context"
	"reflect"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	modctlBackend "github.com/modelpack/modctl/pkg/backend"
	"github.com/modelpack/model-csi-driver/pkg/config/auth"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
)

func TestServiceGetArtifact(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	var getKeyChain func(string) (*auth.PassKeyChain, error)
	var newBackend func(string) (modctlBackend.Backend, error)
	var inspectArtifact func(*ModelArtifact, context.Context, string) (*modctlBackend.InspectedModelArtifact, error)

	patches.ApplyFunc(auth.GetKeyChainByRef, func(ref string) (*auth.PassKeyChain, error) {
		return getKeyChain(ref)
	})
	patches.ApplyFunc(modctlBackend.New, func(storageDir string) (modctlBackend.Backend, error) {
		return newBackend(storageDir)
	})
	patches.ApplyMethod(reflect.TypeOf(&ModelArtifact{}), "Inspect", func(modelArtifact *ModelArtifact, ctx context.Context, ref string) (*modctlBackend.InspectedModelArtifact, error) {
		return inspectArtifact(modelArtifact, ctx, ref)
	})

	t.Run("success", func(t *testing.T) {
		expected := &modctlBackend.InspectedModelArtifact{ID: "sha256:config"}
		getKeyChain = func(ref string) (*auth.PassKeyChain, error) {
			require.Equal(t, "example.com/ns/model:latest", ref)
			return &auth.PassKeyChain{ServerScheme: "http"}, nil
		}
		newBackend = func(string) (modctlBackend.Backend, error) {
			return nil, nil
		}
		inspectArtifact = func(_ *ModelArtifact, _ context.Context, ref string) (*modctlBackend.InspectedModelArtifact, error) {
			require.Equal(t, "example.com/ns/model:latest", ref)
			return expected, nil
		}

		svc := &Service{}
		artifact, err := svc.GetArtifact(context.Background(), "example.com/ns/model:latest")
		require.NoError(t, err)
		require.Same(t, expected, artifact)
	})

	t.Run("get keychain error", func(t *testing.T) {
		getKeyChain = func(string) (*auth.PassKeyChain, error) {
			return nil, errors.New("bad ref")
		}
		newBackend = func(string) (modctlBackend.Backend, error) {
			t.Fatal("backend.New should not be called when auth lookup fails")
			return nil, nil
		}
		inspectArtifact = func(*ModelArtifact, context.Context, string) (*modctlBackend.InspectedModelArtifact, error) {
			t.Fatal("ModelArtifact.Inspect should not be called when auth lookup fails")
			return nil, nil
		}

		svc := &Service{}
		artifact, err := svc.GetArtifact(context.Background(), "bad-ref")
		require.Nil(t, artifact)
		require.Error(t, err)
		require.Contains(t, err.Error(), "get auth for model: bad-ref")
	})

	t.Run("backend new error", func(t *testing.T) {
		getKeyChain = func(string) (*auth.PassKeyChain, error) {
			return &auth.PassKeyChain{ServerScheme: "https"}, nil
		}
		newBackend = func(string) (modctlBackend.Backend, error) {
			return nil, errors.New("backend init failed")
		}
		inspectArtifact = func(*ModelArtifact, context.Context, string) (*modctlBackend.InspectedModelArtifact, error) {
			t.Fatal("ModelArtifact.Inspect should not be called when backend.New fails")
			return nil, nil
		}

		svc := &Service{}
		artifact, err := svc.GetArtifact(context.Background(), "example.com/ns/model:latest")
		require.Nil(t, artifact)
		require.Error(t, err)
		require.Contains(t, err.Error(), "create modctl backend")
	})

	t.Run("inspect error", func(t *testing.T) {
		getKeyChain = func(string) (*auth.PassKeyChain, error) {
			return &auth.PassKeyChain{ServerScheme: "https"}, nil
		}
		newBackend = func(string) (modctlBackend.Backend, error) {
			return nil, nil
		}
		inspectArtifact = func(*ModelArtifact, context.Context, string) (*modctlBackend.InspectedModelArtifact, error) {
			return nil, errors.New("inspect failed")
		}

		svc := &Service{}
		artifact, err := svc.GetArtifact(context.Background(), "example.com/ns/model:latest")
		require.Nil(t, artifact)
		require.Error(t, err)
		require.Contains(t, err.Error(), "inspect failed")
	})
}
