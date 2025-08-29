package auth

import (
	"encoding/base64"
	"fmt"

	"github.com/pkg/errors"

	// nolint
	"github.com/containerd/containerd/reference/docker"
)

// PassKeyChain is user/password based key chain
type PassKeyChain struct {
	Username     string
	Password     string
	ServerScheme string
}

func GetKeyChainByRef(ref string) (*PassKeyChain, error) {
	// nolint
	named, err := docker.ParseDockerRef(ref)
	if err != nil {
		return nil, errors.Wrapf(err, "parse ref %s", ref)
	}

	// nolint
	return FromDockerConfig(docker.Domain(named))
}

func (kc *PassKeyChain) ToBase64() string {
	if kc.Username == "" && kc.Password == "" {
		return ""
	}

	return base64.StdEncoding.EncodeToString(fmt.Appendf(nil, "%s:%s", kc.Username, kc.Password))
}
