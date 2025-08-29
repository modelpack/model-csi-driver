package kmutex

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sync/errgroup"
)

func TestBasic(t *testing.T) {
	kmutex := newKeyMutex()

	val := 0
	eg := errgroup.Group{}
	for i := 1; i <= 1000; i++ {
		eg.Go(func() error {
			kmutex.Lock(context.Background(), "test1")
			defer kmutex.Unlock("test1")
			val++
			return nil
		})
	}
	require.NoError(t, eg.Wait())

	require.Equal(t, 1000, val)
}
