package auth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/pkg/errors"
)

const (
	dockerHost          = "https://index.docker.io/v1/"
	convertedDockerHost = "registry-1.docker.io"
)

type cache struct {
	mutex sync.Mutex
	data  map[string]*PassKeyChain
}

func (c *cache) Get(host string) *PassKeyChain {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.data[host]
}

func (c *cache) Set(host string, auth *PassKeyChain) *PassKeyChain {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.data[host] = auth
	return auth
}

var keyChainCache = cache{
	data: make(map[string]*PassKeyChain),
}

type AuthConfig struct {
	Auth          string `json:"auth,omitempty"`
	Username      string `json:"username,omitempty"`
	Password      string `json:"password,omitempty"`
	ServerAddress string `json:"serveraddress,omitempty"`
	ServerScheme  string `json:"serverscheme,omitempty"`
}

type ConfigFile struct {
	AuthConfigs map[string]AuthConfig `json:"auths"`
}

func (configFile *ConfigFile) GetAuthConfig(host string) *AuthConfig {
	if configFile.AuthConfigs == nil {
		return nil
	}
	authConfig, ok := configFile.AuthConfigs[host]
	if !ok {
		return nil
	}
	return &authConfig
}

func decodeAuth(authStr string) (string, string, error) {
	if authStr == "" {
		return "", "", nil
	}

	decLen := base64.StdEncoding.DecodedLen(len(authStr))
	decoded := make([]byte, decLen)
	authByte := []byte(authStr)
	n, err := base64.StdEncoding.Decode(decoded, authByte)
	if err != nil {
		return "", "", err
	}
	if n > decLen {
		return "", "", errors.Errorf("Something went wrong decoding auth config")
	}
	userName, password, ok := strings.Cut(string(decoded), ":")
	if !ok || userName == "" {
		return "", "", errors.Errorf("Invalid auth configuration file")
	}
	return userName, strings.Trim(password, "\x00"), nil
}

func loadFromReader(configData io.Reader) (*ConfigFile, error) {
	cf := ConfigFile{
		AuthConfigs: make(map[string]AuthConfig),
	}

	if err := json.NewDecoder(configData).Decode(&cf); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	var err error
	for addr, ac := range cf.AuthConfigs {
		if ac.Auth != "" {
			ac.Username, ac.Password, err = decodeAuth(ac.Auth)
			if err != nil {
				return nil, err
			}
		}
		ac.Auth = ""
		ac.ServerAddress = addr
		cf.AuthConfigs[addr] = ac
	}
	return &cf, nil
}

// FromDockerConfig finds auth for a given host in docker's config.json settings.
func FromDockerConfig(host string) (*PassKeyChain, error) {
	if len(host) == 0 {
		return nil, fmt.Errorf("invalid host")
	}

	// The host of docker hub image will be converted to `registry-1.docker.io` in:
	// github.com/containerd/containerd/remotes/docker/registry.go
	// But we need use the key `https://index.docker.io/v1/` to find auth from docker config.
	if host == convertedDockerHost {
		host = dockerHost
	}

	if keyChain := keyChainCache.Get(host); keyChain != nil {
		return keyChain, nil
	}

	dockerConfigPath := "/root/.docker/config.json"
	dockerConfigDir := os.Getenv("DOCKER_CONFIG")
	if dockerConfigDir != "" {
		dockerConfigPath = filepath.Join(dockerConfigDir, "config.json")
	}

	file, err := os.Open(dockerConfigPath)
	if err != nil {
		return nil, errors.Wrapf(err, "open docker config file from %s", dockerConfigPath)
	}
	defer file.Close()

	config, err := loadFromReader(file)
	if err != nil {
		return nil, errors.Wrap(err, "load docker config file")
	}

	authConfig := config.GetAuthConfig(host)
	if authConfig == nil {
		return keyChainCache.Set(host, &PassKeyChain{}), nil
	}

	keyChain := &PassKeyChain{
		Username:     authConfig.Username,
		Password:     authConfig.Password,
		ServerScheme: authConfig.ServerScheme,
	}
	keyChainCache.Set(host, keyChain)

	return keyChain, nil
}
