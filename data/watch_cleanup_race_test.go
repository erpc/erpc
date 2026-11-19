package data

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

// The DynamoDB poller can be inside GetItem when the caller runs cleanup. On
// return it sends the new state on the updates channel. If cleanup closed
// that channel, the send panics in a goroutine with no recover, and the
// process dies. The fake server below holds one poll open until cleanup ran.
func TestDynamoDBConnector_WatchCleanupDuringPollDoesNotPanic(t *testing.T) {
	var calls atomic.Int32
	pollStarted := make(chan struct{})
	releasePoll := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		n := calls.Add(1)
		if n == 2 {
			close(pollStarted)
			<-releasePoll
		}
		// Each answer carries a new state, so the poller always sends it.
		state := fmt.Sprintf(`{"v":%d,"t":%d,"b":"test"}`, n, 1000+n)
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		_, _ = fmt.Fprintf(w, `{"Item":{"pk":{"S":"k"},"rk":{"S":"value"},"value":{"S":%q}}}`, state)
	}))
	defer srv.Close()

	logger := zerolog.New(io.Discard)
	sess, err := session.NewSession(&aws.Config{
		Region:      aws.String("us-east-1"),
		Endpoint:    aws.String(srv.URL),
		Credentials: credentials.NewStaticCredentials("test-key-not-real", "test-secret-not-real", ""),
		MaxRetries:  aws.Int(0),
	})
	require.NoError(t, err)
	d := &DynamoDBConnector{
		id:                "ddb-watch-race",
		logger:            &logger,
		readClient:        dynamodb.New(sess),
		table:             "t",
		partitionKeyName:  "pk",
		rangeKeyName:      "rk",
		statePollInterval: 10 * time.Millisecond,
	}

	updates, cleanup, err := d.WatchCounterInt64(context.Background(), "k")
	require.NoError(t, err)

	select {
	case <-pollStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("the poller never started a poll")
	}
	cleanup()
	close(releasePoll)

	// The channel must close, and the poller must not send after that.
	deadline := time.After(5 * time.Second)
	for open := true; open; {
		select {
		case _, open = <-updates:
		case <-deadline:
			t.Fatal("updates channel did not close after cleanup")
		}
	}
	// Give a late send time to happen, and to crash the binary if it does.
	time.Sleep(200 * time.Millisecond)
	runtime.Gosched()
}

// The PostgreSQL fallback poller exited only when its context ended. A watch
// stopped by cleanup, with its context still live, left the poller running.
// It leaked one goroutine per watch, and its next tick could send on the
// channel cleanup had closed.
func TestPostgreSQLConnector_WatchCleanupStopsTheFallbackPoller(t *testing.T) {
	uri := sharedPostgresURI(t)
	ctx, c := newReadyPgConnector(t, pgConnectorConfig(uri, newPgTable("pg_watch_cleanup")))
	const key = "watch-cleanup"

	// Create the shared listener for the key first, so the count below
	// sees only the per-watch goroutines.
	_, warm, err := c.WatchCounterInt64(ctx, key)
	require.NoError(t, err)
	t.Cleanup(warm)
	time.Sleep(200 * time.Millisecond)
	base := runtime.NumGoroutine()

	const watches = 25
	for i := 0; i < watches; i++ {
		_, cleanup, err := c.WatchCounterInt64(ctx, key)
		require.NoError(t, err)
		cleanup()
	}

	// Other goroutines come and go, so allow slack below one per watch.
	leaked := runtime.NumGoroutine() - base
	for deadline := time.Now().Add(5 * time.Second); leaked >= watches/2 && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
		leaked = runtime.NumGoroutine() - base
	}
	require.Less(t, leaked, watches/2, "goroutines above the base after %d stopped watches", watches)
}
