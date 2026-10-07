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

// Regression: real connector connectTask writer, real initializer state
// transitions and concurrent client reads. Client() is gated on the
// initializer state (an ordered atomic), which alone hides the race, so the
// readers ALSO use currentClient(), the ungated accessor RedisPubSubManager's
// messageLoop reads on every reconnect: with connMu removed from conn() the
// race detector flags these reads against connectTask's write.
func TestRedisConnector_ClientReconnectRace(t *testing.T) {
	m := miniredis.RunT(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lg := zerolog.New(io.Discard)
	cfg := &common.RedisConnectorConfig{URI: "redis://" + m.Addr(), InitTimeout: common.Duration(5 * time.Second), GetTimeout: common.Duration(time.Second), SetTimeout: common.Duration(time.Second)}
	r := &RedisConnector{id: "review", logger: &lg, cfg: cfg, appCtx: ctx, initTimeout: 5 * time.Second, getTimeout: time.Second, setTimeout: time.Second}
	r.initializer = util.NewInitializer(ctx, &lg, &util.InitializerConfig{TaskTimeout: 15 * time.Second, RetryMinDelay: time.Millisecond, RetryMaxDelay: time.Millisecond})
	task := util.NewBootstrapTask("redis-connect/review", r.connectTask)
	if err := r.initializer.ExecuteTasks(ctx, task); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				_ = r.Client()
				_ = r.currentClient()
			}
		}()
	}
	for i := 0; i < 20; i++ {
		// Closing a redis.Client is concurrency safe. It makes the existing
		// connection unhealthy so the production reconnect branch replaces it.
		if c := r.Client(); c != nil {
			_ = c.Close()
		}
		r.initializer.MarkTaskAsFailed(task.Name, errors.New("connection closed"))
		deadline := time.Now().Add(5 * time.Second)
		for {
			err := r.initializer.ExecuteTasks(ctx, task)
			if err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("redis reconnect did not recover: %v", err)
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
	cancel()
	wg.Wait()
}
