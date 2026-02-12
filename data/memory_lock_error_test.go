package data

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

func TestMemoryConnector_Lock_ContentionPreservesContextErr(t *testing.T) {
	ctx := context.Background()

	conn, err := NewMemoryConnector(ctx, &log.Logger, "mem", &common.MemoryConnectorConfig{
		MaxItems:     10,
		MaxTotalSize: "1MB",
	})
	require.NoError(t, err)

	holderCtx, holderCancel := context.WithTimeout(ctx, 2*time.Second)
	defer holderCancel()

	lock1, err := conn.Lock(holderCtx, "k", 2*time.Second)
	require.NoError(t, err)
	require.NotNil(t, lock1)
	defer func() { _ = lock1.Unlock(context.Background()) }()

	contendCtx, contendCancel := context.WithTimeout(ctx, 25*time.Millisecond)
	defer contendCancel()

	lock2, err := conn.Lock(contendCtx, "k", 2*time.Second)
	require.Nil(t, lock2)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrLockContention), "expected ErrLockContention in error chain, got %v", err)
	require.True(t, errors.Is(err, context.DeadlineExceeded), "expected context deadline in error chain, got %v", err)
}

