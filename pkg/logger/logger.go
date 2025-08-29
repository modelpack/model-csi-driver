package logger

import (
	"context"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
)

var (
	logger = logrus.New()
)

type RequestIDKey struct{}
type RequestOpKey struct{}
type RequestVolumeNameKey struct{}
type RequestTargetPathKey struct{}

func NewContext(ctx context.Context, op, volumeName, targetPath string) context.Context {
	ctx = context.WithValue(ctx, RequestIDKey{}, uuid.New().String())
	ctx = context.WithValue(ctx, RequestOpKey{}, op)
	ctx = context.WithValue(ctx, RequestVolumeNameKey{}, volumeName)
	if targetPath != "" {
		ctx = context.WithValue(ctx, RequestTargetPathKey{}, targetPath)
	}
	return ctx
}

func WithContext(ctx context.Context) *logrus.Entry {
	entry := logger.WithField("request", ctx.Value(RequestIDKey{})).
		WithField("op", ctx.Value(RequestOpKey{})).
		WithField("volumeName", ctx.Value(RequestVolumeNameKey{}))

	if ctx.Value(RequestTargetPathKey{}) != nil {
		entry = entry.WithField("targetPath", ctx.Value(RequestTargetPathKey{}))
	}

	return entry
}

func Logger() *logrus.Logger {
	return logger
}
