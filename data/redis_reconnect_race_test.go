package data

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
)

func init() { util.ConfigureTestLogger() }

// Regression: real connector connectTask writer, real
// initializer state transitions, concurrent public borrowed-client getter.
func TestRedisConnector_ClientReconnectRace(t *testing.T) {
	m := miniredis.RunT(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lg := zerolog.New(io.Discard)
	cfg := &common.RedisConnectorConfig{URI: "redis://" + m.Addr(), InitTimeout: common.Duration(time.Second), GetTimeout: common.Duration(time.Second), SetTimeout: common.Duration(time.Second)}
	r := &RedisConnector{id: "review", logger: &lg, cfg: cfg, appCtx: ctx, initTimeout: time.Second, getTimeout: time.Second, setTimeout: time.Second}
	r.initializer = util.NewInitializer(ctx, &lg, &util.InitializerConfig{TaskTimeout: time.Second, RetryMinDelay: time.Nanosecond, RetryMaxDelay: time.Nanosecond})
	task := util.NewBootstrapTask("redis-connect/review", r.connectTask)
	if err := r.initializer.ExecuteTasks(ctx, task); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				_ = r.Client()
				_ = r.checkReady() // also walks Initializer.Errors
			}
		}()
	}
	for i := 0; i < 100; i++ {
		// Closing a redis.Client is concurrency safe. It makes the existing
		// connection unhealthy so the production reconnect branch replaces it.
		if c := r.Client(); c != nil {
			_ = c.Close()
		}
		r.initializer.MarkTaskAsFailed(task.Name, errors.New("connection closed"))
		if err := r.initializer.ExecuteTasks(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	cancel()
	wg.Wait()
}
