package data

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/dynamodb"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var ddbTableSeq atomic.Int64

func ddbConnectorConfig(endpoint string) *common.DynamoDBConnectorConfig {
	return &common.DynamoDBConnectorConfig{
		Endpoint:          endpoint,
		Region:            "us-west-2",
		Table:             fmt.Sprintf("cov_%d_%d", time.Now().UnixNano()%1_000_000, ddbTableSeq.Add(1)),
		PartitionKeyName:  "pk",
		RangeKeyName:      "rk",
		ReverseIndexName:  "idx_reverse",
		TTLAttributeName:  "ttl",
		InitTimeout:       common.Duration(10 * time.Second),
		GetTimeout:        common.Duration(3 * time.Second),
		SetTimeout:        common.Duration(3 * time.Second),
		StatePollInterval: common.Duration(50 * time.Millisecond),
		LockRetryInterval: common.Duration(50 * time.Millisecond),
		Auth: &common.AwsAuthConfig{
			Mode:            "secret",
			AccessKeyID:     "fakeKey",
			SecretAccessKey: "fakeSecret",
		},
	}
}

func newReadyDynamoConnector(t *testing.T, cfg *common.DynamoDBConnectorConfig) (context.Context, *DynamoDBConnector) {
	t.Helper()
	logger := zerolog.New(io.Discard)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	c, err := NewDynamoDBConnector(ctx, &logger, "ddb-"+cfg.Table, cfg)
	require.NoError(t, err)
	require.Equal(t, util.StateReady, c.initializer.State(), "errors: %v", c.initializer.Errors())
	return ctx, c
}

// putRawItem writes an item directly, for shapes Set never produces (legacy
// string values, already-expired TTLs, values with no payload).
func putRawItem(t *testing.T, c *DynamoDBConnector, item map[string]*dynamodb.AttributeValue) {
	t.Helper()
	_, err := c.writeClient.PutItem(&dynamodb.PutItemInput{TableName: aws.String(c.table), Item: item})
	require.NoError(t, err)
}

func ddbItem(pk, rk string) map[string]*dynamodb.AttributeValue {
	return map[string]*dynamodb.AttributeValue{"pk": {S: aws.String(pk)}, "rk": {S: aws.String(rk)}}
}

func pastTTL() *dynamodb.AttributeValue {
	return &dynamodb.AttributeValue{N: aws.String(strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10))}
}

func TestDynamoDBConnector_GetSetDelete(t *testing.T) {
	ctx, c := newReadyDynamoConnector(t, ddbConnectorConfig(sharedDynamoEndpoint(t)))
	assert.Equal(t, "ddb-"+c.table, c.Id())

	ttl := time.Hour
	require.NoError(t, c.Set(ctx, "pk", "with-ttl", []byte("v1"), &ttl))
	require.NoError(t, c.Set(ctx, "pk", "no-ttl", make([]byte, 2048), nil))

	legacy := ddbItem("pk", "legacy")
	legacy["value"] = &dynamodb.AttributeValue{S: aws.String("legacy-string")}
	putRawItem(t, c, legacy)

	expired := ddbItem("pk", "expired")
	expired["value"] = &dynamodb.AttributeValue{B: []byte("old")}
	expired["ttl"] = pastTTL()
	putRawItem(t, c, expired)

	novalue := ddbItem("pk", "novalue")
	novalue["value"] = &dynamodb.AttributeValue{N: aws.String("1")}
	putRawItem(t, c, novalue)

	cases := []struct {
		name    string
		rk      string
		want    string
		errCode common.ErrorCode
		errMsg  string
	}{
		{name: "binary value with ttl", rk: "with-ttl", want: "v1"},
		{name: "legacy string value", rk: "legacy", want: "legacy-string"},
		{name: "missing key", rk: "absent", errCode: common.ErrCodeRecordNotFound},
		{name: "expired ttl", rk: "expired", errCode: common.ErrCodeRecordExpired},
		{name: "value neither binary nor string", rk: "novalue", errMsg: "neither binary nor string"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := c.Get(ctx, ConnectorMainIndex, "pk", tc.rk, nil)
			switch {
			case tc.errCode != "":
				assert.True(t, common.HasErrorCode(err, tc.errCode), "want %s, got %v", tc.errCode, err)
			case tc.errMsg != "":
				require.ErrorContains(t, err, tc.errMsg)
			default:
				require.NoError(t, err)
				assert.Equal(t, tc.want, string(got))
			}
		})
	}

	require.NoError(t, c.Delete(ctx, "pk", "with-ttl"))
	_, err := c.Get(ctx, ConnectorMainIndex, "pk", "with-ttl", nil)
	assert.True(t, common.HasErrorCode(err, common.ErrCodeRecordNotFound), "got %v", err)
}

func TestDynamoDBConnector_ReverseIndexGet(t *testing.T) {
	ctx, c := newReadyDynamoConnector(t, ddbConnectorConfig(sharedDynamoEndpoint(t)))

	require.NoError(t, c.Set(ctx, "evm:1:100", "rk", []byte("b100"), nil))
	require.NoError(t, c.Set(ctx, "evm:1:200", "rk", []byte("b200"), nil))
	legacy := ddbItem("evm:2:100", "rk")
	legacy["value"] = &dynamodb.AttributeValue{S: aws.String("legacy")}
	putRawItem(t, c, legacy)
	// Expired item sorting above all live ones must be skipped.
	expired := ddbItem("evm:1:999", "rk")
	expired["value"] = &dynamodb.AttributeValue{B: []byte("expired")}
	expired["ttl"] = pastTTL()
	putRawItem(t, c, expired)
	// Only an expired item under this range key.
	onlyExpired := ddbItem("evm:1:1", "dead-rk")
	onlyExpired["value"] = &dynamodb.AttributeValue{B: []byte("x")}
	onlyExpired["ttl"] = pastTTL()
	putRawItem(t, c, onlyExpired)
	bad := ddbItem("evm:3:1", "bad-rk")
	bad["value"] = &dynamodb.AttributeValue{N: aws.String("1")}
	putRawItem(t, c, bad)

	cases := []struct {
		name    string
		pk, rk  string
		want    string
		missing bool
		errMsg  string
	}{
		{name: "prefix picks newest live partition", pk: "evm:1:*", rk: "rk", want: "b200"},
		{name: "exact partition", pk: "evm:1:100", rk: "rk", want: "b100"},
		{name: "bare wildcard spans partitions", pk: "*", rk: "rk", want: "legacy"},
		{name: "legacy string value", pk: "evm:2:*", rk: "rk", want: "legacy"},
		{name: "only expired match", pk: "evm:1:*", rk: "dead-rk", missing: true},
		{name: "unknown range key", pk: "", rk: "nope", missing: true},
		{name: "empty range key rejected", pk: "evm:1:*", rk: "", errMsg: "rangeKey must be a non-empty string"},
		{name: "wildcard range key rejected", pk: "evm:1:*", rk: "r*", errMsg: "not contain wildcards"},
		{name: "value neither binary nor string", pk: "evm:3:*", rk: "bad-rk", errMsg: "neither binary nor string"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := c.Get(ctx, ConnectorReverseIndex, tc.pk, tc.rk, nil)
			switch {
			case tc.missing:
				assert.True(t, common.HasErrorCode(err, common.ErrCodeRecordNotFound), "got %v", err)
			case tc.errMsg != "":
				require.ErrorContains(t, err, tc.errMsg)
			default:
				require.NoError(t, err)
				assert.Equal(t, tc.want, string(got))
			}
		})
	}
}

func TestDynamoDBConnector_List(t *testing.T) {
	ctx, c := newReadyDynamoConnector(t, ddbConnectorConfig(sharedDynamoEndpoint(t)))

	require.NoError(t, c.Set(ctx, "a", "1", []byte("va"), nil))
	require.NoError(t, c.Set(ctx, "b", "2", []byte("vb"), nil))
	legacy := ddbItem("c", "3")
	legacy["value"] = &dynamodb.AttributeValue{S: aws.String("vc")}
	putRawItem(t, c, legacy)
	expired := ddbItem("d", "4")
	expired["value"] = &dynamodb.AttributeValue{B: []byte("old")}
	expired["ttl"] = pastTTL()
	putRawItem(t, c, expired)

	collect := func(t *testing.T, index string) map[string]string {
		t.Helper()
		got := map[string]string{}
		token := ""
		for range 20 {
			items, next, err := c.List(ctx, index, 1, token)
			require.NoError(t, err)
			for _, it := range items {
				got[it.PartitionKey+"|"+it.RangeKey] = string(it.Value)
			}
			if next == "" {
				return got
			}
			token = next
		}
		t.Fatal("pagination did not terminate")
		return nil
	}

	want := map[string]string{"a|1": "va", "b|2": "vb", "c|3": "vc"}
	assert.Equal(t, want, collect(t, ConnectorMainIndex), "paged scan returns live items only")
	assert.Equal(t, want, collect(t, ConnectorReverseIndex), "reverse index scan sees the same items")

	t.Run("invalid tokens are rejected", func(t *testing.T) {
		_, _, err := c.List(ctx, ConnectorMainIndex, 1, "%%%")
		require.ErrorContains(t, err, "invalid pagination token")
		_, _, err = c.List(ctx, ConnectorMainIndex, 1, base64.StdEncoding.EncodeToString([]byte("nope")))
		require.ErrorContains(t, err, "invalid pagination token format")
	})
}

func TestDynamoDBConnector_WatchCounter(t *testing.T) {
	ctx, c := newReadyDynamoConnector(t, ddbConnectorConfig(sharedDynamoEndpoint(t)))
	key := "shared/latest"

	initial := CounterInt64State{Value: 1, UpdatedAt: 10, UpdatedBy: "a"}
	b, err := common.SonicCfg.Marshal(initial)
	require.NoError(t, err)
	require.NoError(t, c.Set(ctx, key, "value", b, nil))

	updates, cleanup, err := c.WatchCounterInt64(ctx, key)
	require.NoError(t, err)
	select {
	case st := <-updates:
		assert.Equal(t, initial, st)
	case <-time.After(5 * time.Second):
		t.Fatal("no initial value")
	}

	// Publish is a no-op; the poller picks up stored values instead.
	require.NoError(t, c.PublishCounterInt64(ctx, key, CounterInt64State{Value: 99, UpdatedAt: 99}))

	next := CounterInt64State{Value: 2, UpdatedAt: 20, UpdatedBy: "b"}
	b, err = common.SonicCfg.Marshal(next)
	require.NoError(t, err)
	require.NoError(t, c.Set(ctx, key, "value", b, nil))
	select {
	case st := <-updates:
		assert.Equal(t, next, st, "poller delivers newer stored value")
	case <-time.After(10 * time.Second):
		t.Fatal("poller did not deliver update")
	}
	cleanup()
	_, open := <-updates
	assert.False(t, open)
}

func TestDynamoDBConnector_GetSimpleValueShapes(t *testing.T) {
	ctx, c := newReadyDynamoConnector(t, ddbConnectorConfig(sharedDynamoEndpoint(t)))

	item := func(key string, v *dynamodb.AttributeValue) {
		it := ddbItem(key, "value")
		if v != nil {
			it["value"] = v
		}
		putRawItem(t, c, it)
	}
	item("binary", &dynamodb.AttributeValue{B: []byte(`{"v":1,"t":5}`)})
	item("string", &dynamodb.AttributeValue{S: aws.String(`{"v":2,"t":6}`)})
	item("number", &dynamodb.AttributeValue{N: aws.String("7")})
	item("no-value-attr", nil)
	item("zero-ts", &dynamodb.AttributeValue{B: []byte(`{"v":3,"t":0}`)})
	item("bool", &dynamodb.AttributeValue{BOOL: aws.Bool(true)})

	cases := []struct {
		key  string
		want *CounterInt64State
	}{
		{key: "binary", want: &CounterInt64State{Value: 1, UpdatedAt: 5}},
		{key: "string", want: &CounterInt64State{Value: 2, UpdatedAt: 6}},
		{key: "number"},
		{key: "no-value-attr"},
		{key: "zero-ts"},
		{key: "bool"},
		{key: "absent"},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			st, ok, err := c.getSimpleValue(ctx, tc.key)
			require.NoError(t, err)
			if tc.want == nil {
				assert.False(t, ok)
				return
			}
			require.True(t, ok)
			assert.Equal(t, *tc.want, st)
		})
	}
}

func TestDynamoDBConnector_LockErrors(t *testing.T) {
	ctx, c := newReadyDynamoConnector(t, ddbConnectorConfig(sharedDynamoEndpoint(t)))

	lock, err := c.Lock(ctx, "k", time.Minute)
	require.NoError(t, err)
	assert.False(t, lock.IsNil())

	t.Run("already cancelled context fails before trying", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		_, err := c.Lock(cctx, "other", time.Minute)
		require.ErrorContains(t, err, "cancelled or timed out")
	})

	t.Run("held lock times out while waiting to retry", func(t *testing.T) {
		dctx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err := c.Lock(dctx, "k", time.Minute)
		// The deadline lands either between retries or inside a PutItem
		// attempt; both surface as a lock failure for this key, promptly.
		require.ErrorContains(t, err, "for key 'k'")
		assert.Less(t, time.Since(start), 5*time.Second)
	})

	require.NoError(t, lock.Unlock(ctx))

	var nilLock *dynamoLock
	assert.True(t, nilLock.IsNil())

	t.Run("non-retryable error is returned immediately", func(t *testing.T) {
		cfg := ddbConnectorConfig(sharedDynamoEndpoint(t))
		broken := &DynamoDBConnector{
			logger: c.logger, writeClient: c.writeClient, table: "no_such_table_" + cfg.Table,
			partitionKeyName: "pk", rangeKeyName: "rk", setTimeout: time.Second, lockRetryInterval: time.Millisecond,
		}
		_, err := broken.Lock(ctx, "k", time.Second)
		require.ErrorContains(t, err, "failed to acquire lock for key 'k' during attempt")

		dl := &dynamoLock{connector: broken, lockKey: "k:lock"}
		assert.Error(t, dl.Unlock(ctx), "unlock against a missing table surfaces the error")
	})
}

func TestDynamoDBConnector_EnsuresReverseIndexOnExistingTable(t *testing.T) {
	endpoint := sharedDynamoEndpoint(t)
	cfg := ddbConnectorConfig(endpoint)
	ctx, c := newReadyDynamoConnector(t, cfg)

	// Create a table without the reverse index, then point a connector with a
	// differently-named index at it: the connector must add the index.
	plain := cfg.Table + "_plain"
	_, err := c.writeClient.CreateTable(&dynamodb.CreateTableInput{
		TableName: aws.String(plain),
		KeySchema: []*dynamodb.KeySchemaElement{
			{AttributeName: aws.String("pk"), KeyType: aws.String("HASH")},
			{AttributeName: aws.String("rk"), KeyType: aws.String("RANGE")},
		},
		AttributeDefinitions: []*dynamodb.AttributeDefinition{
			{AttributeName: aws.String("pk"), AttributeType: aws.String("S")},
			{AttributeName: aws.String("rk"), AttributeType: aws.String("S")},
		},
		BillingMode: aws.String("PAY_PER_REQUEST"),
	})
	require.NoError(t, err)

	cfg2 := ddbConnectorConfig(endpoint)
	cfg2.Table = plain
	_, c2 := newReadyDynamoConnector(t, cfg2)
	out, err := c2.writeClient.DescribeTableWithContext(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(plain)})
	require.NoError(t, err)
	var names []string
	for _, g := range out.Table.GlobalSecondaryIndexes {
		names = append(names, *g.IndexName)
	}
	assert.Contains(t, names, "idx_reverse")

	t.Run("no reverse index configured is a no-op", func(t *testing.T) {
		cfg3 := *cfg2
		cfg3.ReverseIndexName = ""
		require.NoError(t, ensureGlobalSecondaryIndexes(ctx, c.logger, c.writeClient, &cfg3))
	})
}

func TestDynamoDBConnector_InitFailures(t *testing.T) {
	logger := zerolog.New(io.Discard)
	newConn := func(t *testing.T, cfg *common.DynamoDBConnectorConfig) *DynamoDBConnector {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		c, err := NewDynamoDBConnector(ctx, &logger, "ddb-init", cfg)
		require.NoError(t, err)
		return c
	}

	fatal := map[string]func(*common.DynamoDBConnectorConfig){
		"missing region":        func(c *common.DynamoDBConnectorConfig) { c.Region = "" },
		"unsupported auth mode": func(c *common.DynamoDBConnectorConfig) { c.Auth.Mode = "bogus" },
		"missing table":         func(c *common.DynamoDBConnectorConfig) { c.Table = "" },
	}
	for name, mutate := range fatal {
		t.Run(name+" is fatal", func(t *testing.T) {
			cfg := ddbConnectorConfig("http://127.0.0.1:1")
			mutate(cfg)
			c := newConn(t, cfg)
			assert.Equal(t, util.StateFatal, c.initializer.State())
		})
	}

	t.Run("every auth mode builds a session", func(t *testing.T) {
		for _, auth := range []*common.AwsAuthConfig{
			nil,
			{Mode: "env"},
			{Mode: "file", CredentialsFile: "/nonexistent", Profile: "default"},
			{Mode: "secret", AccessKeyID: "a", SecretAccessKey: "b"},
		} {
			cfg := ddbConnectorConfig("http://127.0.0.1:1")
			cfg.Auth = auth
			sess, err := createSession(cfg)
			require.NoError(t, err)
			assert.Equal(t, "us-west-2", *sess.Config.Region)
		}
	})

	t.Run("unreachable endpoint returns promptly and entry points report not initialized", func(t *testing.T) {
		cfg := ddbConnectorConfig("http://127.0.0.1:1")
		cfg.InitTimeout = common.Duration(300 * time.Millisecond)
		start := time.Now()
		c := newConn(t, cfg)
		assert.Less(t, time.Since(start), 10*time.Second)
		assert.NotEqual(t, util.StateReady, c.initializer.State())

		bare := &DynamoDBConnector{logger: c.logger}
		ctx := context.Background()
		assert.ErrorContains(t, bare.Set(ctx, "pk", "rk", nil, nil), "not initialized")
		_, err := bare.Get(ctx, ConnectorMainIndex, "pk", "rk", nil)
		assert.ErrorContains(t, err, "not initialized")
		_, err = bare.Lock(ctx, "k", time.Second)
		assert.ErrorContains(t, err, "not initialized")
		assert.ErrorContains(t, bare.Delete(ctx, "pk", "rk"), "not initialized")
		_, _, err = bare.List(ctx, ConnectorMainIndex, 1, "")
		assert.ErrorContains(t, err, "not initialized")
		_, _, err = bare.WatchCounterInt64(ctx, "k")
		assert.ErrorContains(t, err, "not initialized")
	})
}
