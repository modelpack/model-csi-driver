package service

import (
	"context"
	"os"
	"path/filepath"

	"github.com/modelpack/modctl/pkg/backend"
	modctlConfig "github.com/modelpack/modctl/pkg/config"
	"github.com/modelpack/model-csi-driver/pkg/config/auth"
	"github.com/pkg/errors"
)

func (worker *Worker) resolveReferenceDigest(ctx context.Context, reference string) (string, error) {
	keyChain, err := auth.GetKeyChainByRef(reference)
	if err != nil {
		return "", errors.Wrapf(err, "get auth for model: %s", reference)
	}
	plainHTTP := keyChain.ServerScheme == "http"

	b, err := backend.New("")
	if err != nil {
		return "", errors.Wrap(err, "create modctl backend")
	}

	result, err := b.Inspect(ctx, reference, &modctlConfig.Inspect{
		Remote:    true,
		Insecure:  true,
		PlainHTTP: plainHTTP,
	})
	if err != nil {
		return "", errors.Wrapf(err, "inspect model: %s", reference)
	}

	artifact, ok := result.(*backend.InspectedModelArtifact)
	if !ok {
		return "", errors.Errorf("invalid inspected result type for %s", reference)
	}
	if artifact.Digest == "" {
		return "", errors.Errorf("empty digest for %s", reference)
	}

	return artifact.Digest, nil
}

func (worker *Worker) getCachedModelDir(resolvedDigest string) (string, bool, error) {
	sourceModelDir := worker.cfg.Get().GetCacheModelDir(resolvedDigest)
	if _, err := os.Stat(sourceModelDir); err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, errors.Wrapf(err, "stat cache model dir: %s", sourceModelDir)
	}
	return sourceModelDir, true, nil
}

func hardlinkDir(srcDir, dstDir string) error {
	return filepath.Walk(srcDir, func(srcPath string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		relPath, err := filepath.Rel(srcDir, srcPath)
		if err != nil {
			return err
		}
		dstPath := filepath.Join(dstDir, relPath)

		switch mode := info.Mode(); {
		case mode.IsDir():
			return os.MkdirAll(dstPath, mode.Perm())
		case mode.IsRegular():
			if err := os.MkdirAll(filepath.Dir(dstPath), 0755); err != nil {
				return err
			}
			return os.Link(srcPath, dstPath)
		case mode&os.ModeSymlink != 0:
			target, err := os.Readlink(srcPath)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(dstPath), 0755); err != nil {
				return err
			}
			return os.Symlink(target, dstPath)
		default:
			return errors.Errorf("unsupported file type: %s", srcPath)
		}
	})
}

func linkModelDir(sourceModelDir, targetModelDir string) error {
	tmpDir := targetModelDir + ".linking"

	if err := os.RemoveAll(tmpDir); err != nil {
		return errors.Wrapf(err, "remove tmp model dir: %s", tmpDir)
	}

	if err := os.RemoveAll(targetModelDir); err != nil {
		return errors.Wrapf(err, "remove target model dir: %s", targetModelDir)
	}

	if err := hardlinkDir(sourceModelDir, tmpDir); err != nil {
		_ = os.RemoveAll(tmpDir)
		return errors.Wrapf(err, "hardlink model dir from %s to %s", sourceModelDir, tmpDir)
	}

	if err := os.Rename(tmpDir, targetModelDir); err != nil {
		_ = os.RemoveAll(tmpDir)
		return errors.Wrapf(err, "rename tmp model dir to %s", targetModelDir)
	}

	return nil
}
