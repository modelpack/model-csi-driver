package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/modelpack/model-csi-driver/pkg/config"
	"github.com/modelpack/model-csi-driver/pkg/status"
)

func TestPullModel_WithExcludeFilePatterns(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	// This test requires a real model registry to be available
	// Skip if not in CI environment or if test registry not configured
	if os.Getenv("TEST_MODEL_REGISTRY") == "" {
		t.Skip("TEST_MODEL_REGISTRY not set")
	}

	ctx := context.Background()

	// Create a minimal test config
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	cfgContent := `service_name: "model.csi.modelpack.org"
csi_endpoint: "/tmp/csi.sock"
root_dir: "` + tmpDir + `"
pull_config:
  concurrency: 5
`
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0644); err != nil {
		t.Fatalf("Failed to create test config: %v", err)
	}

	cfg, err := config.New(cfgPath)
	if err != nil {
		t.Fatalf("Failed to load config: %v", err)
	}

	sm, err := status.NewStatusManager()
	if err != nil {
		t.Fatalf("Failed to create status manager: %v", err)
	}
	worker, err := NewWorker(cfg, sm)
	if err != nil {
		t.Fatalf("Failed to create worker: %v", err)
	}

	modelDir := filepath.Join(tmpDir, "model")

	testReference := os.Getenv("TEST_MODEL_REFERENCE")
	if testReference == "" {
		testReference = "docker.io/library/test-model:latest"
	}

	excludeFilePatterns := []string{"*.safetensors"}

	// Pull model with file pattern exclusion
	err = worker.PullModel(
		ctx,
		true, // isStaticVolume
		"test-volume",
		"",
		testReference,
		modelDir,
		false, // checkDiskQuota
		false, // excludeModelWeights
		excludeFilePatterns,
	)

	if err != nil {
		t.Fatalf("PullModel failed: %v", err)
	}

	// Verify .safetensors files were excluded
	files, err := os.ReadDir(modelDir)
	if err != nil {
		t.Fatalf("Failed to read model dir: %v", err)
	}

	for _, f := range files {
		if filepath.Ext(f.Name()) == ".safetensors" {
			t.Errorf("Found .safetensors file that should have been excluded: %s", f.Name())
		}
	}

	// Verify other files still exist
	foundConfig := false
	err = filepath.Walk(modelDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && filepath.Ext(path) == ".json" {
			foundConfig = true
		}
		return nil
	})
	if err != nil {
		return
	}

	if !foundConfig {
		t.Error("Expected to find .json files, but none were found")
	}

	// Cleanup
	_ = worker.DeleteModel(ctx, true, "test-volume", "")
}
