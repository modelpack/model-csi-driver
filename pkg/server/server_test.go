package server

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelpack/model-csi-driver/pkg/client"
	"github.com/modelpack/model-csi-driver/pkg/config"
	"github.com/modelpack/model-csi-driver/pkg/service"
	"github.com/modelpack/model-csi-driver/pkg/status"
	modelspec "github.com/modelpack/model-spec/specs-go/v1"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
)

const (
	testImage = "example.com/model:10mb"
	testFile  = "model-1.safetensor"
)

type mockPuller struct {
	pullCfg  *config.PullConfig
	duration time.Duration
	hook     *status.Hook
}

func (puller *mockPuller) Pull(
	ctx context.Context, reference, targetDir string, excludeModelWeights bool,
) error {
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return err
	}

	layerCount := 10
	eg := errgroup.Group{}

	layers := []ocispec.Descriptor{}
	for i := 1; i <= layerCount; i++ {
		layer := ocispec.Descriptor{
			MediaType:   ocispec.MediaTypeImageLayer,
			Digest:      digest.FromString(fmt.Sprintf("test-layer-%d", i)),
			Size:        int64(len(fmt.Sprintf("test-%d", i))),
			Annotations: map[string]string{modelspec.AnnotationFilepath: fmt.Sprintf("model-%d.safetensor", i)},
		}
		layers = append(layers, layer)
	}
	// Mock duplicate layer for testing
	layers[8] = layers[7]

	for i := 1; i <= layerCount; i++ {
		eg.Go(func() error {
			i := i
			layerDesc := layers[i-1]
			puller.hook.BeforePullLayer(layerDesc, ocispec.Manifest{
				Layers: layers,
			})
			fileName := fmt.Sprintf("model-%d.safetensor", i)
			err := os.WriteFile(filepath.Join(targetDir, fileName), []byte(fmt.Sprintf("test-%d", i)), 0644)
			puller.hook.AfterPullLayer(layerDesc, err)
			return err
		})
	}

	if err := eg.Wait(); err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(puller.duration):
		return nil
	}
}

func run(t *testing.T, cmd string) {
	_cmd := exec.Command("sh", "-c", cmd)
	_cmd.Stdout = os.Stdout
	_cmd.Stderr = os.Stderr
	err := _cmd.Run()
	require.Nil(t, err)
}

func testStaticInlineVolume(t *testing.T, ctx context.Context, cfg *config.Config, server *Server, volumeName string) {
	nodeClient, err := client.NewGRPCClient(cfg, cfg.Get().CSIEndpoint)
	require.NoError(t, err)

	// publish static inline volume
	mountedDir := volumeName + "-mounted"
	targetPath := filepath.Join(cfg.Get().RootDir, mountedDir)
	_, err = nodeClient.PublishStaticInlineVolume(ctx, volumeName, targetPath, testImage)
	require.NoError(t, err)

	// check if the volume is published
	statusPath := filepath.Join(cfg.Get().RootDir, "volumes", volumeName, "status.json")
	_, err = os.Stat(statusPath)
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(cfg.Get().RootDir, "volumes", volumeName, "model", testFile))
	require.NoError(t, err)

	// check volume status
	modelStatus, err := server.svc.StatusManager().Get(statusPath)
	require.NoError(t, err)
	require.Equal(t, true, modelStatus.Inline)
	require.Equal(t, volumeName, modelStatus.VolumeName)
	require.Equal(t, "", modelStatus.MountID)
	require.Equal(t, testImage, modelStatus.Reference)
	require.Equal(t, status.StateMounted, modelStatus.State)

	// unmount the volume
	_, err = nodeClient.UnpublishVolume(ctx, volumeName, targetPath)
	require.NoError(t, err)

	// check if the volume is removed
	_, err = os.Stat(filepath.Join(cfg.Get().RootDir, "volumes", volumeName))
	require.True(t, os.IsNotExist(err))
}

func testStaticVolume(t *testing.T, ctx context.Context, cfg *config.Config, server *Server, volumeName string, withTimeout bool) {
	nodeClient, err := client.NewGRPCClient(cfg, cfg.Get().CSIEndpoint)
	require.NoError(t, err)
	controllerClient, err := client.NewGRPCClient(cfg, cfg.Get().ExternalCSIEndpoint)
	require.NoError(t, err)

	// create volume
	resp1, err := controllerClient.CreateVolume(ctx, volumeName, map[string]string{
		cfg.Get().ParameterKeyType():      "image",
		cfg.Get().ParameterKeyReference(): testImage,
	})
	require.NoError(t, err)

	// create again with timeout, the volume should be cleaned up
	if withTimeout {
		ctx, cancel := context.WithTimeout(ctx, time.Second*1)
		defer cancel()
		_, err := controllerClient.CreateVolume(ctx, volumeName, map[string]string{
			cfg.Get().ParameterKeyType():      "image",
			cfg.Get().ParameterKeyReference(): testImage,
		})
		require.True(t, strings.Contains(err.Error(), "DeadlineExceeded"))
		time.Sleep(time.Second * 1)
		_, err = os.Stat(cfg.Get().GetVolumeDir(volumeName))
		require.True(t, os.IsNotExist(err), cfg.Get().GetVolumeDir(volumeName))
		return
	}

	// check if the volume is created
	statusPath := filepath.Join(cfg.Get().RootDir, "volumes", volumeName, "status.json")
	_, err = os.Stat(statusPath)
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(cfg.Get().RootDir, "volumes", volumeName, "model", testFile))
	require.NoError(t, err)

	// check volume status
	modelStatus, err := server.svc.StatusManager().Get(statusPath)
	require.NoError(t, err)
	require.Equal(t, false, modelStatus.Inline)
	require.Equal(t, volumeName, modelStatus.VolumeName)
	require.Equal(t, "", modelStatus.MountID)
	require.Equal(t, testImage, modelStatus.Reference)
	require.Equal(t, status.StatePullSucceeded, modelStatus.State)

	// create volume again with same name
	resp2, err := controllerClient.CreateVolume(ctx, volumeName, map[string]string{
		cfg.Get().ParameterKeyType():      "image",
		cfg.Get().ParameterKeyReference(): testImage,
	})
	require.NoError(t, err)

	// the volume id should be equal
	require.Equal(t, resp1.GetVolume().GetVolumeId(), resp2.GetVolume().GetVolumeId())
	volumeID := resp1.GetVolume().GetVolumeId()

	// mount the volume
	mountedDir := volumeName + "-mounted"
	targetPath := filepath.Join(cfg.Get().RootDir, mountedDir)
	_, err = nodeClient.PublishVolume(ctx, volumeID, targetPath)
	require.NoError(t, err)

	// check if the volume is mounted
	_, err = os.Stat(filepath.Join(cfg.Get().RootDir, mountedDir, testFile))
	require.NoError(t, err)

	// check volume status
	modelStatus, err = server.svc.StatusManager().Get(statusPath)
	require.NoError(t, err)
	require.Equal(t, false, modelStatus.Inline)
	require.Equal(t, volumeName, modelStatus.VolumeName)
	require.Equal(t, "", modelStatus.MountID)
	require.Equal(t, testImage, modelStatus.Reference)
	require.Equal(t, status.StateMounted, modelStatus.State)

	// make mountpoint busy
	file, err := os.Open(filepath.Join(cfg.Get().RootDir, mountedDir, testFile))
	require.NoError(t, err)
	defer func() { _ = file.Close() }()

	// mount the volume again with same volume id
	_, err = nodeClient.PublishVolume(ctx, volumeID, targetPath)
	require.NoError(t, err)

	// unmount the volume
	_, err = nodeClient.UnpublishVolume(ctx, volumeID, targetPath)
	require.NoError(t, err)

	// check if the volume is umounted
	_, err = os.Stat(filepath.Join(cfg.Get().RootDir, "static-volume-1-mounted", testFile))
	require.True(t, os.IsNotExist(err))

	// check volume status
	modelStatus, err = server.svc.StatusManager().Get(statusPath)
	require.NoError(t, err)
	require.Equal(t, false, modelStatus.Inline)
	require.Equal(t, volumeName, modelStatus.VolumeName)
	require.Equal(t, "", modelStatus.MountID)
	require.Equal(t, testImage, modelStatus.Reference)
	require.Equal(t, status.StateUmounted, modelStatus.State)

	// unmount the volume again with same volume id
	_, err = nodeClient.UnpublishVolume(ctx, volumeID, targetPath)
	require.NoError(t, err)

	// delete volume with volume id
	_, err = controllerClient.DeleteVolume(ctx, volumeID)
	require.NoError(t, err)

	// check if the volume is deleted
	_, err = os.Stat(filepath.Join(cfg.Get().RootDir, "volumes", volumeName))
	require.True(t, os.IsNotExist(err))

	// delete volume again with same volume id
	_, err = controllerClient.DeleteVolume(ctx, volumeID)
	require.NoError(t, err)
}

func testDynamicVolume(t *testing.T, ctx context.Context, cfg *config.Config, server *Server, volumeName string, withTimeout bool) {
	nodeClient, err := client.NewGRPCClient(cfg, cfg.Get().CSIEndpoint)
	require.NoError(t, err)

	// mount a dynamic root volume
	mountedDir := volumeName + "-mounted"
	targetPath := filepath.Join(cfg.Get().RootDir, mountedDir)
	_, err = nodeClient.PublishVolume(ctx, volumeName, targetPath)
	require.NoError(t, err)

	// check if the dynamic root volume is mounted
	targetCSISockPath := filepath.Join(targetPath, "csi", "csi.sock")
	_, err = os.Stat(targetCSISockPath)
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(targetPath, "models"))
	require.NoError(t, err)

	// mount the dynamic root volume again
	_, err = nodeClient.PublishVolume(ctx, volumeName, targetPath)
	require.NoError(t, err)

	// check volume status
	statusPath := filepath.Join(filepath.Join(targetPath, "status.json"))
	modelStatus, err := server.svc.StatusManager().Get(statusPath)
	require.NoError(t, err)
	require.Equal(t, false, modelStatus.Inline)
	require.Equal(t, volumeName, modelStatus.VolumeName)
	require.Equal(t, "", modelStatus.MountID)
	require.Equal(t, "", modelStatus.Reference)
	require.Equal(t, "", modelStatus.State)

	// create a dynamic mount with fake volume name
	dynamicHTTPClient, err := client.NewHTTPClient(fmt.Sprintf("unix://%s", targetCSISockPath))
	require.NoError(t, err)
	resp, err := dynamicHTTPClient.CreateMount(ctx, "csi-fake-1", "csi-fake-1-mount-1", testImage, false)
	require.Nil(t, resp)
	require.Error(t, err)

	// create a dynamic mount
	mountID := volumeName + "-mount-1"
	resp, err = dynamicHTTPClient.CreateMount(ctx, volumeName, mountID, testImage, false)
	require.NoError(t, err)
	require.Equal(t, mountID, resp.MountID)
	require.Equal(t, testImage, resp.Reference)
	require.Equal(t, status.StatePullSucceeded, resp.State)

	// create again with timeout, the mount should be cleaned up
	if withTimeout {
		ctx, cancel := context.WithTimeout(ctx, time.Second*1)
		defer cancel()
		_, err := dynamicHTTPClient.CreateMount(ctx, volumeName, mountID, testImage, false)
		require.True(t, strings.Contains(err.Error(), "context deadline exceeded"))
		time.Sleep(time.Second * 1)
		_, err = os.Stat(filepath.Join(targetPath, "models", mountID))
		require.True(t, os.IsNotExist(err))
		return
	}

	// get the dynamic mount
	resp, err = dynamicHTTPClient.GetMount(ctx, volumeName, mountID)
	require.NoError(t, err)
	require.Equal(t, mountID, resp.MountID)
	require.Equal(t, testImage, resp.Reference)
	require.Equal(t, status.StatePullSucceeded, resp.State)

	// check if the dynamic mount is created
	statusPath = filepath.Join(targetPath, "models", mountID, "status.json")
	_, err = os.Stat(statusPath)
	require.NoError(t, err)
	testFilePath := filepath.Join(targetPath, "models", mountID, "model", testFile)
	_, err = os.Stat(testFilePath)
	require.NoError(t, err)

	// create the dynamic mount again
	dynamicHTTPClient, err = client.NewHTTPClient(fmt.Sprintf("unix://%s", targetCSISockPath))
	require.NoError(t, err)
	mountID = volumeName + "-mount-1"
	resp, err = dynamicHTTPClient.CreateMount(ctx, volumeName, mountID, testImage, false)
	require.NoError(t, err)
	require.Equal(t, mountID, resp.MountID)
	require.Equal(t, testImage, resp.Reference)
	require.Equal(t, status.StatePullSucceeded, resp.State)

	// check mount status
	modelStatus, err = server.svc.StatusManager().Get(statusPath)
	require.NoError(t, err)
	require.Equal(t, false, modelStatus.Inline)
	require.Equal(t, volumeName, modelStatus.VolumeName)
	require.Equal(t, mountID, modelStatus.MountID)
	require.Equal(t, testImage, modelStatus.Reference)
	require.Equal(t, status.StatePullSucceeded, modelStatus.State)

	// reuse the mount id with different reference
	_, err = dynamicHTTPClient.CreateMount(ctx, volumeName, mountID, testImage+"-new", false)
	require.Error(t, err)

	// create another dynamic mount
	mountID2 := volumeName + "-mount-2"
	resp, err = dynamicHTTPClient.CreateMount(ctx, volumeName, mountID2, testImage+"-1", false)
	require.NoError(t, err)
	require.Equal(t, mountID2, resp.MountID)
	require.Equal(t, testImage+"-1", resp.Reference)
	require.Equal(t, status.StatePullSucceeded, resp.State)

	// get the second dynamic mount
	resp, err = dynamicHTTPClient.GetMount(ctx, volumeName, mountID2)
	require.NoError(t, err)
	require.Equal(t, mountID2, resp.MountID)
	require.Equal(t, testImage+"-1", resp.Reference)
	require.Equal(t, status.StatePullSucceeded, resp.State)

	// list all dynamic mounts
	mounts, err := dynamicHTTPClient.ListMounts(ctx, volumeName)
	require.NoError(t, err)
	for idx := range mounts {
		mounts[idx].Progress = status.Progress{}
	}
	require.Equal(t, []status.Status{
		{
			VolumeName: volumeName,
			MountID:    mountID,
			Reference:  testImage,
			State:      status.StatePullSucceeded,
		},
		{
			VolumeName: volumeName,
			MountID:    mountID2,
			Reference:  testImage + "-1",
			State:      status.StatePullSucceeded,
		},
	}, mounts)

	// make mountpoint busy
	file, err := os.Open(testFilePath)
	require.NoError(t, err)
	defer func() { _ = file.Close() }()

	// delete the dynamic mount
	err = dynamicHTTPClient.DeleteMount(ctx, volumeName, mountID)
	require.NoError(t, err)

	// check if the dynamic mount is deleted
	_, err = os.Stat(filepath.Join(targetPath, "models", mountID))
	require.True(t, os.IsNotExist(err))

	// check mount status
	_, err = dynamicHTTPClient.GetMount(ctx, volumeName, mountID)
	require.True(t, strings.Contains(err.Error(), "NOT_FOUND"))

	// delete the dynamic mount again
	err = dynamicHTTPClient.DeleteMount(ctx, volumeName, mountID)
	require.NoError(t, err)

	// list all dynamic mounts again
	mounts, err = dynamicHTTPClient.ListMounts(ctx, volumeName)
	require.NoError(t, err)
	for idx := range mounts {
		mounts[idx].Progress = status.Progress{}
	}
	require.Equal(t, []status.Status{
		{
			VolumeName: volumeName,
			MountID:    mountID2,
			Reference:  testImage + "-1",
			State:      status.StatePullSucceeded,
		},
	}, mounts)

	// unmount the dynamic root volume
	_, err = nodeClient.UnpublishVolume(ctx, volumeName, targetPath)
	require.NoError(t, err)

	// check if the dynamic root volume is unmounted
	_, err = os.Stat(filepath.Join(cfg.Get().RootDir, volumeName))
	require.True(t, os.IsNotExist(err))

	// unmount the dynamic root volume again
	_, err = nodeClient.UnpublishVolume(ctx, volumeName, targetPath)
	require.NoError(t, err)
}

func testBasicVolume(
	t *testing.T, ctx context.Context, cfg *config.Config,
	server *Server, count int, concurrent bool,
) {
	eg := errgroup.Group{}
	for i := 0; i < count; i++ {
		i := i
		if concurrent {
			eg.Go(func() error {
				testStaticVolume(t, ctx, cfg, server, fmt.Sprintf("pvc-static-volume-%d", i), false)
				testStaticVolume(t, ctx, cfg, server, fmt.Sprintf("pvc-static-volume-%d", i), true)
				testDynamicVolume(t, ctx, cfg, server, fmt.Sprintf("csi-dynamic-volume-%d", i), false)
				testDynamicVolume(t, ctx, cfg, server, fmt.Sprintf("csi-dynamic-volume-%d", i), true)
				testStaticInlineVolume(t, ctx, cfg, server, fmt.Sprintf("csi-static-inline-volume-%d", i))
				return nil
			})
		} else {
			testStaticVolume(t, ctx, cfg, server, fmt.Sprintf("pvc-static-volume-%d", i), false)
			testStaticVolume(t, ctx, cfg, server, fmt.Sprintf("pvc-static-volume-%d", i), true)
			testDynamicVolume(t, ctx, cfg, server, fmt.Sprintf("csi-dynamic-volume-%d", i), false)
			testDynamicVolume(t, ctx, cfg, server, fmt.Sprintf("csi-dynamic-volume-%d", i), true)
			testStaticInlineVolume(t, ctx, cfg, server, fmt.Sprintf("csi-static-inline-volume-%d", i))
		}
	}
	require.NoError(t, eg.Wait())
}

func testStaticConcurrentVolume(t *testing.T, cfg *config.Config, server *Server, concurrent int) {
	eg := errgroup.Group{}

	for i := 0; i < concurrent; i++ {
		eg.Go(func() error {
			controllerClient, err := client.NewGRPCClient(cfg, cfg.Get().ExternalCSIEndpoint)
			require.NoError(t, err)
			_, err = controllerClient.CreateVolume(context.TODO(), "pvc-test", map[string]string{
				cfg.Get().ParameterKeyType():      "image",
				cfg.Get().ParameterKeyReference(): testImage,
			})
			if err != nil && strings.Contains(err.Error(), "context canceled") {
				return nil
			}
			require.NoError(t, err)
			return nil
		})
	}

	for i := 0; i < concurrent; i++ {
		eg.Go(func() error {
			controllerClient, err := client.NewGRPCClient(cfg, cfg.Get().ExternalCSIEndpoint)
			require.NoError(t, err)
			_, err = controllerClient.DeleteVolume(context.TODO(), "pvc-test")
			require.NoError(t, err)
			return nil
		})
	}

	require.NoError(t, eg.Wait())
}

func testDynamicConcurrentVolume(t *testing.T, cfg *config.Config, server *Server, concurrent int) {
	nodeClient, err := client.NewGRPCClient(cfg, cfg.Get().CSIEndpoint)
	require.NoError(t, err)

	volumeName := "csi-volume-test-1"
	mountID := "mount-1"

	// mount a dynamic root volume
	mountedDir := volumeName + "-mounted"
	targetPath := filepath.Join(cfg.Get().RootDir, mountedDir)
	_, err = nodeClient.PublishVolume(context.Background(), volumeName, targetPath)
	require.NoError(t, err)
	targetCSISockPath := filepath.Join(targetPath, "csi", "csi.sock")

	eg := errgroup.Group{}

	for i := 0; i < concurrent; i++ {
		eg.Go(func() error {
			dynamicHTTPClient, err := client.NewHTTPClient(fmt.Sprintf("unix://%s", targetCSISockPath))
			require.NoError(t, err)

			// create a dynamic volume
			mountID := volumeName + "-mount-1"
			_, err = dynamicHTTPClient.CreateMount(context.Background(), volumeName, mountID, testImage, false)
			require.NoError(t, err)

			return nil
		})
	}

	for i := 0; i < concurrent; i++ {
		eg.Go(func() error {
			dynamicHTTPClient, err := client.NewHTTPClient(fmt.Sprintf("unix://%s", targetCSISockPath))
			require.NoError(t, err)

			// get the dynamic volume
			_, err = dynamicHTTPClient.GetMount(context.Background(), volumeName, mountID)
			if err != nil && strings.Contains(err.Error(), "NOT_FOUND") {
				return nil
			}
			require.NoError(t, err)

			// list all dynamic volumes
			_, err = dynamicHTTPClient.ListMounts(context.Background(), volumeName)
			require.NoError(t, err)

			// delete the dynamic volume
			err = dynamicHTTPClient.DeleteMount(context.Background(), volumeName, mountID)
			require.NoError(t, err)

			return nil
		})
	}

	require.NoError(t, eg.Wait())

	// unmount the dynamic root volume
	_, err = nodeClient.UnpublishVolume(context.Background(), volumeName, targetPath)
	require.NoError(t, err)
}

func TestServer(t *testing.T) {
	require.NoError(t, os.Setenv("X_CSI_MODE", "node"))
	require.NoError(t, os.Setenv("CSI_NODE_ID", "test-node-id"))
	rootDir, err := os.MkdirTemp("/tmp", "model-csi-driver")
	require.NoError(t, err)
	defer func() { _ = os.RemoveAll(rootDir) }()

	defaultCoofigPath := "../../test/testdata/config.test.yaml"
	configPathFromEnv := os.Getenv("CONFIG_PATH")
	if configPathFromEnv != "" {
		defaultCoofigPath = configPathFromEnv
	}
	cfg, err := config.New(defaultCoofigPath)
	require.NoError(t, err)
	cfg.Get().RootDir = rootDir
	cfg.Get().PullConfig.ProxyURL = ""
	service.CacheScanInterval = 1 * time.Second

	service.NewPuller = func(ctx context.Context, pullCfg *config.PullConfig, hook *status.Hook, diskQuotaChecker *service.DiskQuotaChecker) service.Puller {
		return &mockPuller{
			pullCfg:  pullCfg,
			duration: time.Second * 2,
			hook:     hook,
		}
	}

	ctx := context.TODO()
	server, err := NewServer(cfg)
	require.NoError(t, err)
	go func() {
		err := server.Run(ctx)
		require.NoError(t, err)
	}()

	time.Sleep(time.Second * 2)

	testBasicVolume(t, ctx, cfg, server, 5, false)
	testBasicVolume(t, ctx, cfg, server, 5, true)
	testStaticConcurrentVolume(t, cfg, server, 5)
	testDynamicConcurrentVolume(t, cfg, server, 5)

	run(t, "curl http://127.0.0.1:5244/metrics | grep -v '# '")
}
