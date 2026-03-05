package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRawConfig_ParameterKeys(t *testing.T) {
	cfg := &RawConfig{ServiceName: "test.csi.example.com"}

	require.Equal(t, "test.csi.example.com/type", cfg.ParameterKeyType())
	require.Equal(t, "test.csi.example.com/reference", cfg.ParameterKeyReference())
	require.Equal(t, "test.csi.example.com/mount-id", cfg.ParameterKeyMountID())
	require.Equal(t, "test.csi.example.com/status/state", cfg.ParameterKeyStatusState())
	require.Equal(t, "test.csi.example.com/status/progress", cfg.ParameterKeyStatusProgress())
	require.Equal(t, "test.csi.example.com/node-ip", cfg.ParameterVolumeContextNodeIP())
	require.Equal(t, "test.csi.example.com/check-disk-quota", cfg.ParameterKeyCheckDiskQuota())
	require.Equal(t, "test.csi.example.com/exclude-model-weights", cfg.ParameterKeyExcludeModelWeights())
	require.Equal(t, "test.csi.example.com/exclude-file-patterns", cfg.ParameterKeyExcludeFilePatterns())
}

func TestRawConfig_PathHelpers(t *testing.T) {
	cfg := &RawConfig{
		ServiceName: "test.csi.example.com",
		RootDir:     "/var/lib/model-csi",
	}

	require.Equal(t, "/var/lib/model-csi/volumes", cfg.GetVolumesDir())
	require.Equal(t, "/var/lib/model-csi/volumes/pvc-vol", cfg.GetVolumeDir("pvc-vol"))
	require.Equal(t, "/var/lib/model-csi/volumes/pvc-vol/model", cfg.GetModelDir("pvc-vol"))
	require.Equal(t, "/var/lib/model-csi/volumes/csi-vol", cfg.GetVolumeDirForDynamic("csi-vol"))
	require.Equal(t, "/var/lib/model-csi/volumes/csi-vol/models", cfg.GetModelsDirForDynamic("csi-vol"))
	require.Equal(t, "/var/lib/model-csi/volumes/csi-vol/models/mnt-1", cfg.GetMountIDDirForDynamic("csi-vol", "mnt-1"))
	require.Equal(t, "/var/lib/model-csi/volumes/csi-vol/models/mnt-1/model", cfg.GetModelDirForDynamic("csi-vol", "mnt-1"))
	require.Equal(t, "/var/lib/model-csi/volumes/csi-vol/csi", cfg.GetCSISockDirForDynamic("csi-vol"))
	require.Equal(t, "/var/lib/model-csi/volumes/csi-vol/csi/csi.sock", cfg.GetCSISockPathForDynamic("csi-vol"))
}

func TestRawConfig_ModeHelpers(t *testing.T) {
	controller := &RawConfig{Mode: "controller"}
	require.True(t, controller.IsControllerMode())
	require.False(t, controller.IsNodeMode())

	node := &RawConfig{Mode: "node"}
	require.False(t, node.IsControllerMode())
	require.True(t, node.IsNodeMode())

	empty := &RawConfig{}
	require.False(t, empty.IsControllerMode())
	require.False(t, empty.IsNodeMode())
}

func TestHumanizeSize_UnmarshalYAML(t *testing.T) {
	// Direct test of the HumanizeSize type.
	var hs HumanizeSize
	err := hs.UnmarshalYAML(func(v interface{}) error {
		*(v.(*string)) = "1GiB"
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, HumanizeSize(1073741824), hs)
}

func TestHumanizeSize_UnmarshalYAML_Invalid(t *testing.T) {
	var hs HumanizeSize
	err := hs.UnmarshalYAML(func(v interface{}) error {
		*(v.(*string)) = "not-a-size"
		return nil
	})
	require.Error(t, err)
}

func TestNewWithRaw(t *testing.T) {
	rawCfg := &RawConfig{ServiceName: "test-svc", RootDir: "/tmp/test"}
	cfg := NewWithRaw(rawCfg)
	require.NotNil(t, cfg)
	require.Equal(t, "test-svc", cfg.Get().ServiceName)
}
