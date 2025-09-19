package config

import (
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/dustin/go-humanize"
	"github.com/modelpack/model-csi-driver/pkg/logger"
	"github.com/pkg/errors"
	"gopkg.in/yaml.v2"
)

type HumanizeSize uint64

func (s *HumanizeSize) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var str string
	if err := unmarshal(&str); err != nil {
		return err
	}

	size, err := humanize.ParseBytes(str)
	if err != nil {
		return err
	}

	*s = HumanizeSize(size)

	return nil
}

type RawConfig struct {
	// Pattern:
	// 	static: /var/lib/dragonfly/model-csi/volumes/$volumeName/model
	// dynamic: /var/lib/dragonfly/model-csi/volumes/$volumeName/models
	//          /var/lib/dragonfly/model-csi/volumes/$volumeName/csi.sock
	ServiceName              string     `yaml:"service_name"`
	RootDir                  string     `yaml:"root_dir"`
	ExternalCSIEndpoint      string     `yaml:"external_csi_endpoint"`
	ExternalCSIAuthorization string     `yaml:"external_csi_authorization"`
	DynamicCSIEndpoint       string     `yaml:"dynamic_csi_endpoint"`
	CSIEndpoint              string     `yaml:"csi_endpoint"`
	MetricsAddr              string     `yaml:"metrics_addr"`
	TraceEndpoint            string     `yaml:"trace_endpoint"`
	PprofAddr                string     `yaml:"pprof_addr"`
	PullConfig               PullConfig `yaml:"pull_config"`
	Features                 Features   `yaml:"features"`
	NodeID                   string     // From env CSI_NODE_ID
	Mode                     string     // From env X_CSI_MODE: "controller" or "node"
}

type Features struct {
	CheckDiskQuota bool         `yaml:"check_disk_quota"`
	DiskUsageLimit HumanizeSize `yaml:"disk_usage_limit"`
}

type PullConfig struct {
	DockerConfigDir           string `yaml:"docker_config_dir"`
	ProxyURL                  string `yaml:"proxy_url"`
	DragonflyEndpoint         string `yaml:"dragonfly_endpoint"`
	Concurrency               uint   `yaml:"concurrency"`
	PullLayerTimeoutInSeconds uint   `yaml:"pull_layer_timeout_in_seconds"`
}

func (cfg *RawConfig) ParameterKeyType() string {
	return cfg.ServiceName + "/type"
}

func (cfg *RawConfig) ParameterKeyReference() string {
	return cfg.ServiceName + "/reference"
}

func (cfg *RawConfig) ParameterKeyMountID() string {
	return cfg.ServiceName + "/mount-id"
}

func (cfg *RawConfig) ParameterKeyStatusState() string {
	return cfg.ServiceName + "/status/state"
}

func (cfg *RawConfig) ParameterKeyStatusProgress() string {
	return cfg.ServiceName + "/status/progress"
}

func (cfg *RawConfig) ParameterVolumeContextNodeIP() string {
	return cfg.ServiceName + "/node-ip"
}

func (cfg *RawConfig) ParameterKeyCheckDiskQuota() string {
	return cfg.ServiceName + "/check-disk-quota"
}

// /var/lib/dragonfly/model-csi/volumes
func (cfg *RawConfig) GetVolumesDir() string {
	return filepath.Join(cfg.RootDir, "volumes")
}

// /var/lib/dragonfly/model-csi/volumes/$volumeName
func (cfg *RawConfig) GetVolumeDir(volumeName string) string {
	return filepath.Join(cfg.GetVolumesDir(), volumeName)
}

// /var/lib/dragonfly/model-csi/volumes/$volumeName/model
func (cfg *RawConfig) GetModelDir(volumeName string) string {
	return filepath.Join(cfg.GetVolumesDir(), volumeName, "model")
}

// /var/lib/dragonfly/model-csi/volumes/$volumeName
func (cfg *RawConfig) GetVolumeDirForDynamic(volumeName string) string {
	return filepath.Join(cfg.GetVolumesDir(), volumeName)
}

// /var/lib/dragonfly/model-csi/volumes/$volumeName/models
func (cfg *RawConfig) GetModelsDirForDynamic(volumeName string) string {
	return filepath.Join(cfg.GetVolumeDirForDynamic(volumeName), "models")
}

// /var/lib/dragonfly/model-csi/volumes/$volumeName/models/$mountID
func (cfg *RawConfig) GetMountIDDirForDynamic(volumeName, mountID string) string {
	return filepath.Join(cfg.GetVolumeDirForDynamic(volumeName), "models", mountID)
}

// /var/lib/dragonfly/model-csi/volumes/$volumeName/models/$mountID/model
func (cfg *RawConfig) GetModelDirForDynamic(volumeName, mountID string) string {
	return filepath.Join(cfg.GetVolumeDirForDynamic(volumeName), "models", mountID, "model")
}

// /var/lib/dragonfly/model-csi/volumes/$volumeName/csi
func (cfg *RawConfig) GetCSISockDirForDynamic(volumeName string) string {
	return filepath.Join(cfg.GetVolumeDirForDynamic(volumeName), "csi")
}

func (cfg *RawConfig) IsControllerMode() bool {
	return cfg.Mode == "controller"
}

func (cfg *RawConfig) IsNodeMode() bool {
	return cfg.Mode == "node"
}

func parse(path string) (*RawConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.Wrap(err, "read config file")
	}

	var cfg RawConfig
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, errors.Wrap(err, "unmarshal config file")
	}

	if cfg.ServiceName == "" {
		return nil, errors.New("service_name is required")
	}

	csiMode := os.Getenv("X_CSI_MODE")
	if csiMode == "" {
		return nil, errors.New("X_CSI_MODE env is required")
	}
	if csiMode != "controller" && csiMode != "node" {
		return nil, errors.New("X_CSI_MODE env must be controller or node")
	}
	cfg.Mode = csiMode

	if cfg.CSIEndpoint == "" {
		return nil, errors.New("csi_endpoint is required")
	}

	if cfg.IsNodeMode() {
		csiNodeID := os.Getenv("CSI_NODE_ID")
		if csiNodeID == "" {
			return nil, errors.New("CSI_NODE_ID env is required")
		}
		cfg.NodeID = csiNodeID

		if cfg.PullConfig.DockerConfigDir == "" {
			fromEnv := os.Getenv("DOCKER_CONFIG")
			if fromEnv != "" {
				cfg.PullConfig.DockerConfigDir = fromEnv
			} else {
				cfg.PullConfig.DockerConfigDir = "/root/.docker"
			}
		}

		if err := os.Setenv("DOCKER_CONFIG", cfg.PullConfig.DockerConfigDir); err != nil {
			return nil, errors.Wrap(err, "set DOCKER_CONFIG env")
		}

		if cfg.RootDir == "" {
			return nil, errors.New("root_dir is required")
		}

		if cfg.PullConfig.DragonflyEndpoint != "" {
			endpoint, err := url.Parse(cfg.PullConfig.DragonflyEndpoint)
			if err != nil {
				return nil, errors.Wrap(err, "parse dragonfly endpoint")
			}
			if endpoint.Path == "" {
				return nil, errors.New("pull_config.dragonfly_endpoint must be a valid URL with path")
			}
			if _, err := os.Stat(endpoint.Path); err != nil {
				return nil, errors.Wrapf(err, "check dragonfly endpoint: %s", endpoint.Path)
			}
		}
	}

	return &cfg, nil
}

type Config struct {
	atomic.Value
}

func New(path string) (*Config, error) {
	cfg, err := parse(path)
	if err != nil {
		return nil, err
	}

	atomicCfg := NewWithRaw(cfg)

	go atomicCfg.watch(path)

	return atomicCfg, nil
}

func NewWithRaw(cfg *RawConfig) *Config {
	atomicCfg := &Config{
		Value: atomic.Value{},
	}
	atomicCfg.Store(cfg)
	return atomicCfg
}

func (cfg *Config) Get() *RawConfig {
	return cfg.Load().(*RawConfig)
}

func (cfg *Config) reload(path string) {
	newCfg, err := parse(path)
	if err != nil {
		logger.Logger().WithError(err).Error("failed to parse config file")
		return
	}

	mutex.Lock()
	defer mutex.Unlock()

	cfg.Store(newCfg)

	logger.Logger().Infof("config reloaded: %s", path)
}
