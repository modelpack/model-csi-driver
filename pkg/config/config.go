package config

import (
	"net/url"
	"os"
	"path/filepath"

	"github.com/dustin/go-humanize"
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

type Config struct {
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
	TraceEndpooint           string     `yaml:"trace_endpoint"`
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

func (cfg *Config) ParameterKeyType() string {
	return cfg.ServiceName + "/type"
}

func (cfg *Config) ParameterKeyReference() string {
	return cfg.ServiceName + "/reference"
}

func (cfg *Config) ParameterKeyMountID() string {
	return cfg.ServiceName + "/mount-id"
}

func (cfg *Config) ParameterKeyStatusState() string {
	return cfg.ServiceName + "/status/state"
}

func (cfg *Config) ParameterKeyStatusProgress() string {
	return cfg.ServiceName + "/status/progress"
}

func (cfg *Config) ParameterVolumeContextNodeIP() string {
	return cfg.ServiceName + "/node-ip"
}

func (cfg *Config) ParameterKeyCheckDiskQuota() string {
	return cfg.ServiceName + "/check-disk-quota"
}

// /var/lib/dragonfly/model-csi/volumes
func (cfg *Config) GetVolumesDir() string {
	return filepath.Join(cfg.RootDir, "volumes")
}

// /var/lib/dragonfly/model-csi/volumes/$volumeName
func (cfg *Config) GetVolumeDir(volumeName string) string {
	return filepath.Join(cfg.GetVolumesDir(), volumeName)
}

// /var/lib/dragonfly/model-csi/volumes/$volumeName/model
func (cfg *Config) GetModelDir(volumeName string) string {
	return filepath.Join(cfg.GetVolumesDir(), volumeName, "model")
}

// /var/lib/dragonfly/model-csi/volumes/$volumeName
func (cfg *Config) GetVolumeDirForDynamic(volumeName string) string {
	return filepath.Join(cfg.GetVolumesDir(), volumeName)
}

// /var/lib/dragonfly/model-csi/volumes/$volumeName/models
func (cfg *Config) GetModelsDirForDynamic(volumeName string) string {
	return filepath.Join(cfg.GetVolumeDirForDynamic(volumeName), "models")
}

// /var/lib/dragonfly/model-csi/volumes/$volumeName/models/$mountID
func (cfg *Config) GetMountIDDirForDynamic(volumeName, mountID string) string {
	return filepath.Join(cfg.GetVolumeDirForDynamic(volumeName), "models", mountID)
}

// /var/lib/dragonfly/model-csi/volumes/$volumeName/models/$mountID/model
func (cfg *Config) GetModelDirForDynamic(volumeName, mountID string) string {
	return filepath.Join(cfg.GetVolumeDirForDynamic(volumeName), "models", mountID, "model")
}

// /var/lib/dragonfly/model-csi/volumes/$volumeName/csi
func (cfg *Config) GetCSISockDirForDynamic(volumeName string) string {
	return filepath.Join(cfg.GetVolumeDirForDynamic(volumeName), "csi")
}

func (cfg *Config) IsControllerMode() bool {
	return cfg.Mode == "controller"
}

func (cfg *Config) IsNodeMode() bool {
	return cfg.Mode == "node"
}

func parse(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.Wrap(err, "read config file")
	}

	var cfg Config
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

func New(path string) (*Config, error) {
	cfg, err := parse(path)
	if err != nil {
		return nil, err
	}

	go cfg.watch(path)

	return cfg, nil
}
