package server

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/CloudNativeAI/model-csi-driver/pkg/config"
	"github.com/CloudNativeAI/model-csi-driver/pkg/service"
	modelStatus "github.com/CloudNativeAI/model-csi-driver/pkg/status"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/labstack/echo/v4"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type HttpHandler struct {
	cfg *config.Config
	svc *service.Service
}

func checkIdentifier(identifier string) bool {
	if identifier == "" {
		return false
	}
	matched, err := regexp.MatchString("^[a-zA-Z0-9_-]+$", identifier)
	if err != nil {
		return false
	}
	return matched
}

func handleError(c echo.Context, err error) error {
	if e, ok := status.FromError(err); ok && e.Code() == codes.InvalidArgument {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Code:    ERR_CODE_INVALID_ARGUMENT,
			Message: e.Message(),
		})
	} else if ok && e.Code() == codes.ResourceExhausted {
		return c.JSON(http.StatusNotAcceptable, ErrorResponse{
			Code:    ERR_CODE_INSUFFICIENT_DISK_QUOTA,
			Message: e.Message(),
		})
	}
	return c.JSON(http.StatusInternalServerError, ErrorResponse{
		Code:    ERR_CODE_INTERNAL,
		Message: err.Error(),
	})
}

func (h *HttpHandler) CreateVolume(c echo.Context) error {
	volumeName := c.Param("volume_name")

	if !checkIdentifier(volumeName) {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Code:    ERR_CODE_INVALID_ARGUMENT,
			Message: "volume_name is invalid",
		})
	}

	req := new(service.MountRequest)
	if err := c.Bind(req); err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Code:    ERR_CODE_INVALID_ARGUMENT,
			Message: "invalid JSON body",
		})
	}

	req.MountID = strings.TrimSpace(req.MountID)
	req.Reference = strings.TrimSpace(req.Reference)

	if !checkIdentifier(req.MountID) {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Code:    ERR_CODE_INVALID_ARGUMENT,
			Message: "mount_id is invalid",
		})
	}

	if req.Reference == "" {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Code:    ERR_CODE_INVALID_ARGUMENT,
			Message: "reference is invalid",
		})
	}

	_, err := h.svc.CreateVolume(c.Request().Context(), &csi.CreateVolumeRequest{
		Name: volumeName,
		Parameters: map[string]string{
			h.cfg.ParameterKeyType():           "image",
			h.cfg.ParameterKeyReference():      req.Reference,
			h.cfg.ParameterKeyMountID():        req.MountID,
			h.cfg.ParameterKeyCheckDiskQuota(): strconv.FormatBool(req.CheckDiskQuota),
		},
	})
	if err != nil {
		return handleError(c, err)
	}

	mount := modelStatus.Status{
		VolumeName: volumeName,
		MountID:    req.MountID,
		Reference:  req.Reference,
		State:      modelStatus.StatePullSucceeded,
	}

	return c.JSON(http.StatusCreated, mount)
}

func (h *HttpHandler) GetVolume(c echo.Context) error {
	volumeName := c.Param("volume_name")
	mountID := c.Param("mount_id")

	if !checkIdentifier(volumeName) {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Code:    ERR_CODE_INVALID_ARGUMENT,
			Message: "volume_name is invalid",
		})
	}

	if !checkIdentifier(mountID) {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Code:    ERR_CODE_INVALID_ARGUMENT,
			Message: "mount_id is invalid",
		})
	}

	status, err := h.svc.GetDynamicVolume(c.Request().Context(), volumeName, mountID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return c.JSON(http.StatusNotFound, ErrorResponse{
				Code:    ERR_CODE_NOT_FOUND,
				Message: fmt.Sprintf("volume_name %s with mount_id %s is not found", volumeName, mountID),
			})
		}
		return handleError(c, err)
	}

	return c.JSON(http.StatusOK, status)
}

func (h *HttpHandler) DeleteVolume(c echo.Context) error {
	volumeName := c.Param("volume_name")
	mountID := c.Param("mount_id")

	if !checkIdentifier(volumeName) {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Code:    ERR_CODE_INVALID_ARGUMENT,
			Message: "volume_name is invalid",
		})
	}

	if !checkIdentifier(mountID) {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Code:    ERR_CODE_INVALID_ARGUMENT,
			Message: "mount_id is invalid",
		})
	}

	volumeID := fmt.Sprintf("%s/%s", volumeName, mountID)
	_, err := h.svc.DeleteVolume(c.Request().Context(), &csi.DeleteVolumeRequest{
		VolumeId: volumeID,
	})
	if err != nil {
		return handleError(c, err)
	}

	return c.JSON(http.StatusNoContent, nil)
}

func (h *HttpHandler) ListVolumes(c echo.Context) error {
	volumeName := c.Param("volume_name")

	if !checkIdentifier(volumeName) {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Code:    ERR_CODE_INVALID_ARGUMENT,
			Message: "volume_name is invalid",
		})
	}

	statuses, err := h.svc.ListDynamicVolumes(c.Request().Context(), volumeName)
	if err != nil {
		return handleError(c, err)
	}

	return c.JSON(http.StatusOK, statuses)
}
