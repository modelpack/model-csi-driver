package client

import (
	"context"
	"fmt"
	"net/http"

	"github.com/CloudNativeAI/model-csi-driver/pkg/service"
	"github.com/CloudNativeAI/model-csi-driver/pkg/status"
)

func (client *HTTPClient) CreateMount(ctx context.Context, volumeName, mountID, reference string, checkDiskQuota bool) (*status.Status, error) {
	req := service.MountRequest{
		MountID:        mountID,
		Reference:      reference,
		CheckDiskQuota: checkDiskQuota,
	}

	var mountItem status.Status
	if _, err := client.request(
		ctx,
		http.MethodPost,
		fmt.Sprintf("/api/v1/volumes/%s/mounts", volumeName),
		&req,
		nil,
		&mountItem,
	); err != nil {
		return nil, err
	}

	return &mountItem, nil
}

func (client *HTTPClient) GetMount(ctx context.Context, volumeName, mountID string) (*status.Status, error) {
	var mountItem status.Status
	if _, err := client.request(
		ctx,
		http.MethodGet,
		fmt.Sprintf("/api/v1/volumes/%s/mounts/%s", volumeName, mountID),
		nil,
		nil,
		&mountItem,
	); err != nil {
		return nil, err
	}

	return &mountItem, nil
}

func (client *HTTPClient) DeleteMount(ctx context.Context, volumeName, mountID string) error {
	if _, err := client.request(
		ctx,
		http.MethodDelete,
		fmt.Sprintf("/api/v1/volumes/%s/mounts/%s", volumeName, mountID),
		nil,
		nil,
		nil,
	); err != nil {
		return err
	}

	return nil
}

func (client *HTTPClient) ListMounts(ctx context.Context, volumeName string) ([]status.Status, error) {
	var mountItems []status.Status

	if _, err := client.request(
		ctx,
		http.MethodGet,
		fmt.Sprintf("/api/v1/volumes/%s/mounts", volumeName),
		nil,
		nil,
		&mountItems,
	); err != nil {
		return nil, err
	}

	return mountItems, nil
}
