package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/labstack/echo/v4"
	"github.com/modelpack/model-csi-driver/pkg/config"
	modelStatus "github.com/modelpack/model-csi-driver/pkg/status"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"oras.land/oras-go/v2/errdef"
)

type DynamicServerHandler struct {
	cfg *config.Config
	svc *Service
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

func (h *DynamicServerHandler) CreateVolume(c echo.Context) error {
	volumeName := c.Param("volume_name")

	if !checkIdentifier(volumeName) {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Code:    ERR_CODE_INVALID_ARGUMENT,
			Message: "volume_name is invalid",
		})
	}

	req := new(MountRequest)
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

	excludeFilePatternsJSON, err := json.Marshal(req.ExcludeFilePatterns)
	if err != nil {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Code:    ERR_CODE_INVALID_ARGUMENT,
			Message: "invalid exclude_file_patterns",
		})
	}

	_, err = h.svc.CreateVolume(c.Request().Context(), &csi.CreateVolumeRequest{
		Name: volumeName,
		Parameters: map[string]string{
			h.cfg.Get().ParameterKeyType():                "image",
			h.cfg.Get().ParameterKeyReference():           req.Reference,
			h.cfg.Get().ParameterKeyMountID():             req.MountID,
			h.cfg.Get().ParameterKeyCheckDiskQuota():      strconv.FormatBool(req.CheckDiskQuota),
			h.cfg.Get().ParameterKeyExcludeModelWeights(): strconv.FormatBool(req.ExcludeModelWeights),
			h.cfg.Get().ParameterKeyExcludeFilePatterns(): string(excludeFilePatternsJSON),
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

func (h *DynamicServerHandler) GetVolume(c echo.Context) error {
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

func (h *DynamicServerHandler) DeleteVolume(c echo.Context) error {
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

func (h *DynamicServerHandler) ListVolumes(c echo.Context) error {
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

func (h *DynamicServerHandler) GetArtifact(c echo.Context) error {
	reference := c.Param("reference")

	if reference == "" {
		return c.JSON(http.StatusBadRequest, ErrorResponse{
			Code:    ERR_CODE_INVALID_ARGUMENT,
			Message: "reference is invalid",
		})
	}

	artifact, err := h.svc.GetArtifact(c.Request().Context(), reference)
	if err != nil {
		if errors.Is(err, errdef.ErrNotFound) {
			return c.JSON(http.StatusNotFound, ErrorResponse{
				Code:    ERR_CODE_NOT_FOUND,
				Message: fmt.Sprintf("artifact with reference %s is not found", reference),
			})
		}
		return handleError(c, err)
	}

	return c.JSON(http.StatusOK, artifact)
}
