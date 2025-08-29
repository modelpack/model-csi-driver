package metrics

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/modelpack/model-csi-driver/pkg/logger"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const EnvPodIP = "POD_IP"

type Server struct {
	listener net.Listener
	addr     string
}

var defaultHost = "0.0.0.0"

func GetAddrByEnv(addr string, local bool) string {
	if local {
		addr = strings.Replace(addr, "$POD_IP", "127.0.0.1", 1)
	} else if envPodIP := os.Getenv(EnvPodIP); envPodIP != "" {
		addr = strings.Replace(addr, "$POD_IP", envPodIP, 1)
	} else {
		addr = strings.Replace(addr, "$POD_IP", defaultHost, 1)
	}
	return addr
}

func NewServer(addr string) (*Server, error) {
	if addr == "" {
		return nil, fmt.Errorf("metrics addr is required")
	}

	url, err := url.Parse(addr)
	if err != nil {
		return nil, errors.Wrapf(err, "parse metrics addr: %s", addr)
	}
	host := url.Hostname()
	port := url.Port()
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%s", host, port))
	if err != nil {
		return nil, fmt.Errorf("error listening on %s: %v", addr, err)
	}

	return &Server{
		listener: ln,
		addr:     addr,
	}, nil
}

func (s *Server) Serve(stop <-chan struct{}) {
	mux := http.NewServeMux()

	handler := promhttp.HandlerFor(Registry, promhttp.HandlerOpts{
		ErrorHandling: promhttp.HTTPErrorOnError,
	})
	detailHandler := promhttp.HandlerFor(DetailRegistry, promhttp.HandlerOpts{
		ErrorHandling: promhttp.HTTPErrorOnError,
	})
	mux.Handle("/metrics", handler)
	mux.Handle("/metrics/detail", detailHandler)

	server := http.Server{
		Handler: mux,
	}

	go func() {
		if err := server.Serve(s.listener); err != nil && err != http.ErrServerClosed {
			logger.Logger().WithError(err).Errorf("serve metrics server: %s", s.addr)
		}
	}()

	<-stop

	if err := server.Shutdown(context.Background()); err != nil {
		logger.Logger().WithError(err).Errorf("stop metrics server: %s", s.addr)
	}
}
