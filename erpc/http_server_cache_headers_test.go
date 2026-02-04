package erpc

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/stretchr/testify/require"
)

func TestSetResponseHeaders_CacheAgeHeaders(t *testing.T) {
	ctx := context.Background()

	t.Run("CacheHitWithCachedAtSetsHeaders", func(t *testing.T) {
		w := httptest.NewRecorder()
		resp := common.NewNormalizedResponse()
		resp.SetFromCache(true)
		cachedAt := time.Now().Add(-5 * time.Second).Unix()
		resp.SetCacheStoredAtUnix(cachedAt)

		setResponseHeaders(ctx, resp, w)

		headers := w.Result().Header
		require.Equal(t, "HIT", headers.Get("X-ERPC-Cache"))
		require.Equal(t, fmt.Sprintf("%d", cachedAt), headers.Get("X-ERPC-Cache-At"))

		ageHeader := headers.Get("X-ERPC-Cache-Age")
		require.NotEmpty(t, ageHeader)
		age, err := strconv.ParseInt(ageHeader, 10, 64)
		require.NoError(t, err)
		require.GreaterOrEqual(t, age, int64(0))
	})

	t.Run("CacheHitWithoutCachedAtSkipsAgeHeaders", func(t *testing.T) {
		w := httptest.NewRecorder()
		resp := common.NewNormalizedResponse()
		resp.SetFromCache(true)

		setResponseHeaders(ctx, resp, w)

		headers := w.Result().Header
		require.Equal(t, "HIT", headers.Get("X-ERPC-Cache"))
		require.Empty(t, headers.Get("X-ERPC-Cache-Age"))
		require.Empty(t, headers.Get("X-ERPC-Cache-At"))
	})

	t.Run("NonCacheSkipsAgeHeadersEvenWithCachedAt", func(t *testing.T) {
		w := httptest.NewRecorder()
		resp := common.NewNormalizedResponse()
		resp.SetCacheStoredAtUnix(time.Now().Add(-10 * time.Second).Unix())

		setResponseHeaders(ctx, resp, w)

		headers := w.Result().Header
		require.Equal(t, "MISS", headers.Get("X-ERPC-Cache"))
		require.Empty(t, headers.Get("X-ERPC-Cache-Age"))
		require.Empty(t, headers.Get("X-ERPC-Cache-At"))
	})

	t.Run("FutureCachedAtClampsAgeToZero", func(t *testing.T) {
		w := httptest.NewRecorder()
		resp := common.NewNormalizedResponse()
		resp.SetFromCache(true)
		cachedAt := time.Now().Add(5 * time.Minute).Unix()
		resp.SetCacheStoredAtUnix(cachedAt)

		setResponseHeaders(ctx, resp, w)

		headers := w.Result().Header
		require.Equal(t, "HIT", headers.Get("X-ERPC-Cache"))
		require.Equal(t, fmt.Sprintf("%d", cachedAt), headers.Get("X-ERPC-Cache-At"))
		require.Equal(t, "0", headers.Get("X-ERPC-Cache-Age"))
	})
}
