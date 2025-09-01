package service

import (
	"context"
	"fmt"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func loadKubeConfig() (*kubernetes.Clientset, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, errors.Wrap(err, "failed to load in-cluster config")
	}
	config.QPS = 100
	config.Burst = 100

	return kubernetes.NewForConfig(config)
}

func (s *Service) getNode(ctx context.Context, nodeName string) (*corev1.Node, error) {
	return s.node.Get(ctx, nodeName, metav1.GetOptions{})
}

type nodeInfo struct {
	ip       string
	hostname string
}

func getNodeInfo(node *corev1.Node) (*nodeInfo, error) {
	var (
		ip       string
		hostname string
		ok       bool
	)
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP && addr.Address != "" {
			ip = addr.Address
			break
		}
	}

	if ip == "" {
		return nil, fmt.Errorf("node internal ip not exist")
	}

	// nolint:staticcheck
	hostname, ok = node.ObjectMeta.Labels[labelHostname]
	if !ok {
		return nil, fmt.Errorf("node hostname not exist")
	}

	return &nodeInfo{
		ip:       ip,
		hostname: hostname,
	}, nil
}
