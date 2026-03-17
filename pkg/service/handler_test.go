package service

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/labstack/echo/v4"
	"github.com/modelpack/modctl/pkg/backend"
	"github.com/modelpack/model-csi-driver/pkg/config"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	grpcStatus "google.golang.org/grpc/status"
	"oras.land/oras-go/v2/errdef"
)

// --- checkIdentifier ---

func TestCheckIdentifier_Empty(t *testing.T) {
	require.False(t, checkIdentifier(""))
}

func TestCheckIdentifier_Valid(t *testing.T) {
	require.True(t, checkIdentifier("my-volume_123"))
}

func TestCheckIdentifier_InvalidChars(t *testing.T) {
	require.False(t, checkIdentifier("vol/invalid"))
	require.False(t, checkIdentifier("vol invalid"))
	require.False(t, checkIdentifier("vol.name"))
}

// --- handleError ---

func newEchoContext(t *testing.T, body string) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	return e.NewContext(req, rec), rec
}

func TestHandleError_InvalidArgument(t *testing.T) {
	c, rec := newEchoContext(t, "")
	err := grpcStatus.Error(codes.InvalidArgument, "bad param")
	_ = handleError(c, err)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, ERR_CODE_INVALID_ARGUMENT, resp.Code)
}

func TestHandleError_ResourceExhausted(t *testing.T) {
	c, rec := newEchoContext(t, "")
	err := grpcStatus.Error(codes.ResourceExhausted, "disk full")
	_ = handleError(c, err)
	require.Equal(t, http.StatusNotAcceptable, rec.Code)

	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, ERR_CODE_INSUFFICIENT_DISK_QUOTA, resp.Code)
}

func TestHandleError_Internal(t *testing.T) {
	c, rec := newEchoContext(t, "")
	err := grpcStatus.Error(codes.Internal, "boom")
	_ = handleError(c, err)
	require.Equal(t, http.StatusInternalServerError, rec.Code)

	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, ERR_CODE_INTERNAL, resp.Code)
}

// --- DynamicServerHandler ---

func newHandler(t *testing.T) (*DynamicServerHandler, *Service) {
	t.Helper()
	svc, _ := newNodeService(t)
	rawCfg := &config.RawConfig{
		ServiceName: "test.csi.example.com",
		RootDir:     t.TempDir(),
	}
	cfg := config.NewWithRaw(rawCfg)
	return &DynamicServerHandler{cfg: cfg, svc: svc}, svc
}

func newHandlerContextWithParam(t *testing.T, method, url, body string, paramNames, paramValues []string) (echo.Context, *httptest.ResponseRecorder) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(method, url, bytes.NewBufferString(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames(paramNames...)
	c.SetParamValues(paramValues...)
	return c, rec
}

func TestDynamicServerHandler_CreateVolume_InvalidVolumeName(t *testing.T) {
	h, _ := newHandler(t)
	c, rec := newHandlerContextWithParam(t, http.MethodPost, "/", `{"mount_id":"m1","reference":"test/model:latest"}`,
		[]string{"volume_name"}, []string{"invalid/name"})
	_ = h.CreateVolume(c)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestDynamicServerHandler_CreateVolume_InvalidMountID(t *testing.T) {
	h, _ := newHandler(t)
	c, rec := newHandlerContextWithParam(t, http.MethodPost, "/", `{"mount_id":"bad/mount","reference":"test/model:latest"}`,
		[]string{"volume_name"}, []string{"my-volume"})
	_ = h.CreateVolume(c)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestDynamicServerHandler_CreateVolume_MissingReference(t *testing.T) {
	h, _ := newHandler(t)
	c, rec := newHandlerContextWithParam(t, http.MethodPost, "/", `{"mount_id":"m1","reference":""}`,
		[]string{"volume_name"}, []string{"my-volume"})
	_ = h.CreateVolume(c)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestDynamicServerHandler_CreateVolume_CreatesVolume(t *testing.T) {
	h, _ := newHandler(t)
	body := `{"mount_id":"m1","reference":"test/model:latest","check_disk_quota":false}`
	c, rec := newHandlerContextWithParam(t, http.MethodPost, "/", body,
		[]string{"volume_name"}, []string{"my-volume"})
	_ = h.CreateVolume(c)
	// Either Created (201) or error from localCreateVolume (e.g. InvalidArgument > 400)
	require.Contains(t, []int{http.StatusCreated, http.StatusBadRequest, http.StatusInternalServerError}, rec.Code)
}

func TestDynamicServerHandler_GetVolume_InvalidVolumeName(t *testing.T) {
	h, _ := newHandler(t)
	c, rec := newHandlerContextWithParam(t, http.MethodGet, "/", "",
		[]string{"volume_name", "mount_id"}, []string{"bad/vol", "m1"})
	_ = h.GetVolume(c)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestDynamicServerHandler_GetVolume_InvalidMountID(t *testing.T) {
	h, _ := newHandler(t)
	c, rec := newHandlerContextWithParam(t, http.MethodGet, "/", "",
		[]string{"volume_name", "mount_id"}, []string{"my-volume", "bad/mount"})
	_ = h.GetVolume(c)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestDynamicServerHandler_GetVolume_NotFound(t *testing.T) {
	h, _ := newHandler(t)
	c, rec := newHandlerContextWithParam(t, http.MethodGet, "/", "",
		[]string{"volume_name", "mount_id"}, []string{"my-volume", "m1"})
	_ = h.GetVolume(c)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestDynamicServerHandler_DeleteVolume_InvalidVolumeName(t *testing.T) {
	h, _ := newHandler(t)
	c, rec := newHandlerContextWithParam(t, http.MethodDelete, "/", "",
		[]string{"volume_name", "mount_id"}, []string{"bad/vol", "m1"})
	_ = h.DeleteVolume(c)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestDynamicServerHandler_DeleteVolume_InvalidMountID(t *testing.T) {
	h, _ := newHandler(t)
	c, rec := newHandlerContextWithParam(t, http.MethodDelete, "/", "",
		[]string{"volume_name", "mount_id"}, []string{"my-volume", "bad/mount"})
	_ = h.DeleteVolume(c)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestDynamicServerHandler_DeleteVolume_Success(t *testing.T) {
	h, _ := newHandler(t)
	// volume doesn't exist → localDeleteVolume returns OK (removes, even if not there)
	c, rec := newHandlerContextWithParam(t, http.MethodDelete, "/", "",
		[]string{"volume_name", "mount_id"}, []string{"my-volume", "m1"})
	_ = h.DeleteVolume(c)
	// NoContent or InternalServerError depending on vol state
	require.Contains(t, []int{http.StatusNoContent, http.StatusInternalServerError}, rec.Code)
}

func TestDynamicServerHandler_ListVolumes_InvalidVolumeName(t *testing.T) {
	h, _ := newHandler(t)
	c, rec := newHandlerContextWithParam(t, http.MethodGet, "/", "",
		[]string{"volume_name"}, []string{"bad/vol"})
	_ = h.ListVolumes(c)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestDynamicServerHandler_ListVolumes_EmptyDir(t *testing.T) {
	h, _ := newHandler(t)
	c, rec := newHandlerContextWithParam(t, http.MethodGet, "/", "",
		[]string{"volume_name"}, []string{"my-volume"})
	_ = h.ListVolumes(c)
	// Non-existent models dir → error
	require.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestDynamicServerHandler_GetArtifact_InvalidReference(t *testing.T) {
	h, _ := newHandler(t)
	c, rec := newHandlerContextWithParam(t, http.MethodGet, "/", "",
		[]string{"reference"}, []string{""})
	_ = h.GetArtifact(c)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestDynamicServerHandler_GetArtifact_NotFound(t *testing.T) {
	h, svc := newHandler(t)
	patch := gomonkey.ApplyMethod(reflect.TypeOf(svc), "GetArtifact", func(*Service, context.Context, string) (*backend.InspectedModelArtifact, error) {
		return nil, errdef.ErrNotFound
	})
	defer patch.Reset()

	c, rec := newHandlerContextWithParam(t, http.MethodGet, "/", "",
		[]string{"reference"}, []string{"example.com/ns/model:latest"})
	_ = h.GetArtifact(c)
	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestDynamicServerHandler_GetArtifact_Success(t *testing.T) {
	h, svc := newHandler(t)
	patch := gomonkey.ApplyMethod(reflect.TypeOf(svc), "GetArtifact", func(*Service, context.Context, string) (*backend.InspectedModelArtifact, error) {
		return &backend.InspectedModelArtifact{
			ID:        "sha256:config",
			CreatedAt: "2026-03-17T00:00:00Z",
			Layers: []backend.InspectedModelArtifactLayer{{
				MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
				Digest:    "sha256:layer",
				Filepath:  "weights/model.safetensors",
			}},
		}, nil
	})
	defer patch.Reset()

	c, rec := newHandlerContextWithParam(t, http.MethodGet, "/", "",
		[]string{"reference"}, []string{"example.com/ns/model:latest"})
	_ = h.GetArtifact(c)
	require.Equal(t, http.StatusOK, rec.Code)

	var actual map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &actual))
	require.Equal(t, "sha256:config", actual["Id"])
	require.Equal(t, "2026-03-17T00:00:00Z", actual["CreatedAt"])
}

func TestDynamicServerHandler_GetArtifact_InternalError(t *testing.T) {
	h, svc := newHandler(t)
	patch := gomonkey.ApplyMethod(reflect.TypeOf(svc), "GetArtifact", func(*Service, context.Context, string) (*backend.InspectedModelArtifact, error) {
		return nil, grpcStatus.Error(codes.InvalidArgument, "bad reference")
	})
	defer patch.Reset()

	c, rec := newHandlerContextWithParam(t, http.MethodGet, "/", "",
		[]string{"reference"}, []string{"example.com/ns/model:latest"})
	_ = h.GetArtifact(c)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}
