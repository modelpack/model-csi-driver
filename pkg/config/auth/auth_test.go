package auth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// ─── decodeAuth ───────────────────────────────────────────────────────────────

func TestDecodeAuth_Empty(t *testing.T) {
	user, pass, err := decodeAuth("")
	require.NoError(t, err)
	require.Empty(t, user)
	require.Empty(t, pass)
}

func TestDecodeAuth_Valid(t *testing.T) {
	raw := base64.StdEncoding.EncodeToString([]byte("myuser:mypass"))
	user, pass, err := decodeAuth(raw)
	require.NoError(t, err)
	require.Equal(t, "myuser", user)
	require.Equal(t, "mypass", pass)
}

func TestDecodeAuth_InvalidBase64(t *testing.T) {
	_, _, err := decodeAuth("!!!not-base64!!!")
	require.Error(t, err)
}

func TestDecodeAuth_NoColon(t *testing.T) {
	raw := base64.StdEncoding.EncodeToString([]byte("nocolon"))
	_, _, err := decodeAuth(raw)
	require.Error(t, err)
}

// ─── loadFromReader ───────────────────────────────────────────────────────────

func TestLoadFromReader_Valid(t *testing.T) {
	user := "testuser"
	pass := "testpass"
	auth := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("%s:%s", user, pass)))

	cfg := ConfigFile{
		AuthConfigs: map[string]AuthConfig{
			"registry.example.com": {Auth: auth},
		},
	}
	data, err := json.Marshal(cfg)
	require.NoError(t, err)

	result, err := loadFromReader(strings.NewReader(string(data)))
	require.NoError(t, err)
	require.NotNil(t, result)
	ac := result.GetAuthConfig("registry.example.com")
	require.NotNil(t, ac)
	require.Equal(t, user, ac.Username)
	require.Equal(t, pass, ac.Password)
}

func TestLoadFromReader_InvalidJSON(t *testing.T) {
	_, err := loadFromReader(strings.NewReader("{invalid json"))
	require.Error(t, err)
}

func TestLoadFromReader_Empty(t *testing.T) {
	result, err := loadFromReader(strings.NewReader(""))
	require.NoError(t, err)
	require.NotNil(t, result)
}

// ─── ConfigFile.GetAuthConfig ─────────────────────────────────────────────────

func TestGetAuthConfig_Found(t *testing.T) {
	cf := &ConfigFile{
		AuthConfigs: map[string]AuthConfig{
			"registry.example.com": {Username: "user", Password: "pass"},
		},
	}
	ac := cf.GetAuthConfig("registry.example.com")
	require.NotNil(t, ac)
	require.Equal(t, "user", ac.Username)
}

func TestGetAuthConfig_NotFound(t *testing.T) {
	cf := &ConfigFile{
		AuthConfigs: map[string]AuthConfig{},
	}
	ac := cf.GetAuthConfig("nonexistent.registry.io")
	require.Nil(t, ac)
}

func TestGetAuthConfig_NilMap(t *testing.T) {
	cf := &ConfigFile{}
	ac := cf.GetAuthConfig("registry.example.com")
	require.Nil(t, ac)
}

// ─── PassKeyChain.ToBase64 ────────────────────────────────────────────────────

func TestToBase64_Empty(t *testing.T) {
	kc := &PassKeyChain{}
	result := kc.ToBase64()
	require.Empty(t, result)
}

func TestToBase64_WithCredentials(t *testing.T) {
	kc := &PassKeyChain{Username: "admin", Password: "secret"}
	result := kc.ToBase64()
	require.NotEmpty(t, result)

	decoded, err := base64.StdEncoding.DecodeString(result)
	require.NoError(t, err)
	require.Equal(t, "admin:secret", string(decoded))
}

// ─── FromDockerConfig / GetKeyChainByRef ──────────────────────────────────────

func TestFromDockerConfig_EmptyHost(t *testing.T) {
	_, err := FromDockerConfig("")
	require.Error(t, err)
}

func TestFromDockerConfig_WithDockerConfigFile(t *testing.T) {
	tmpDir := t.TempDir()
	dockerConfigPath := filepath.Join(tmpDir, "config.json")

	auth := base64.StdEncoding.EncodeToString([]byte("user1:pass1"))
	configContent := fmt.Sprintf(`{"auths":{"registry.test.io":{"auth":"%s"}}}`, auth)
	require.NoError(t, os.WriteFile(dockerConfigPath, []byte(configContent), 0600))

	// Reset cache for this host.
	keyChainCache.mutex.Lock()
	delete(keyChainCache.data, "registry.test.io")
	keyChainCache.mutex.Unlock()

	t.Setenv("DOCKER_CONFIG", tmpDir)

	kc, err := FromDockerConfig("registry.test.io")
	require.NoError(t, err)
	require.Equal(t, "user1", kc.Username)
	require.Equal(t, "pass1", kc.Password)
}

func TestFromDockerConfig_MissingConfigFile(t *testing.T) {
	// Reset cache for host.
	host := "missing.registry.io"
	keyChainCache.mutex.Lock()
	delete(keyChainCache.data, host)
	keyChainCache.mutex.Unlock()

	t.Setenv("DOCKER_CONFIG", "/nonexistent/dir")
	_, err := FromDockerConfig(host)
	require.Error(t, err)
}

func TestFromDockerConfig_DockerHubConversion(t *testing.T) {
	tmpDir := t.TempDir()
	dockerConfigPath := filepath.Join(tmpDir, "config.json")

	auth := base64.StdEncoding.EncodeToString([]byte("dockeruser:dockerpass"))
	configContent := fmt.Sprintf(`{"auths":{"%s":{"auth":"%s"}}}`, dockerHost, auth)
	require.NoError(t, os.WriteFile(dockerConfigPath, []byte(configContent), 0600))

	// Clear cache.
	keyChainCache.mutex.Lock()
	delete(keyChainCache.data, dockerHost)
	delete(keyChainCache.data, convertedDockerHost)
	keyChainCache.mutex.Unlock()

	t.Setenv("DOCKER_CONFIG", tmpDir)

	// registry-1.docker.io should be converted to dockerHost internally.
	kc, err := FromDockerConfig(convertedDockerHost)
	require.NoError(t, err)
	require.Equal(t, "dockeruser", kc.Username)
}

func TestGetKeyChainByRef_Valid(t *testing.T) {
	tmpDir := t.TempDir()
	dockerConfigPath := filepath.Join(tmpDir, "config.json")
	auth := base64.StdEncoding.EncodeToString([]byte("refuser:refpass"))
	configContent := fmt.Sprintf(`{"auths":{"ghcr.io":{"auth":"%s"}}}`, auth)
	require.NoError(t, os.WriteFile(dockerConfigPath, []byte(configContent), 0600))

	keyChainCache.mutex.Lock()
	delete(keyChainCache.data, "ghcr.io")
	keyChainCache.mutex.Unlock()

	t.Setenv("DOCKER_CONFIG", tmpDir)

	kc, err := GetKeyChainByRef("ghcr.io/my-org/model:v1")
	require.NoError(t, err)
	require.NotNil(t, kc)
}

func TestGetKeyChainByRef_InvalidRef(t *testing.T) {
	_, err := GetKeyChainByRef(":::invalid:::")
	require.Error(t, err)
}
