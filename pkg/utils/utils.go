package utils

import (
	"context"
	"time"

	"github.com/modelpack/model-csi-driver/pkg/logger"
	"github.com/pkg/errors"
)

var ErrBreakRetry = errors.New("break retry")

func WithRetry(ctx context.Context, handle func() error, total int, delay time.Duration) error {
	for {
		total--
		err := handle()
		if err == nil || errors.Is(err, ErrBreakRetry) {
			return err
		}

		if total > 0 {
			logger.WithContext(ctx).Warnf("retry (remain %d times) after %s", total, delay)
			time.Sleep(delay)
			continue
		}

		return err
	}
}
