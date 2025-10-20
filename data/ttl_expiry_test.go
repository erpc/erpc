package data

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog/log"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	util.ConfigureTestLogger()
}

// TestConnectorTTLExpiry tests that expired entries are not returned
// for all connector types, especially through the reverse index path
func TestConnectorTTLExpiry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	testCases := []struct {
		name             string
		createConnector  func(t *testing.T) Connector
		skipReverseIndex bool // Some connectors don't support reverse index well
	}{
		{
			name: "DynamoDB",
			createConnector: func(t *testing.T) Connector {
				if os.Getenv("DYNAMODB_TEST_ENDPOINT") == "" {
					t.Skip("Skipping DynamoDB test - DYNAMODB_TEST_ENDPOINT not set")
				}
				lg := log.Logger
				cfg := &common.DynamoDBConnectorConfig{
					Endpoint:          os.Getenv("DYNAMODB_TEST_ENDPOINT"),
					Region:            "us-east-1",
					Table:             fmt.Sprintf("test_ttl_%d", time.Now().Unix()),
					PartitionKeyName:  "pk",
					RangeKeyName:      "rk",
					ReverseIndexName:  "idx_reverse",
					TTLAttributeName:  "ttl",
					InitTimeout:       common.Duration(10 * time.Second),
					GetTimeout:        common.Duration(5 * time.Second),
					SetTimeout:        common.Duration(5 * time.Second),
					StatePollInterval: common.Duration(1 * time.Second),
					LockRetryInterval: common.Duration(100 * time.Millisecond),
				}
				conn, err := NewDynamoDBConnector(ctx, &lg, "test-dynamo", cfg)
				require.NoError(t, err)
				require.NotNil(t, conn)

				// Wait for initialization
				time.Sleep(2 * time.Second)
				return conn
			},
		},
		{
			name: "PostgreSQL",
			createConnector: func(t *testing.T) Connector {
				if os.Getenv("POSTGRES_TEST_URI") == "" {
					t.Skip("Skipping PostgreSQL test - POSTGRES_TEST_URI not set")
				}
				lg := log.Logger
				cfg := &common.PostgreSQLConnectorConfig{
					ConnectionUri: os.Getenv("POSTGRES_TEST_URI"),
					Table:         fmt.Sprintf("test_ttl_%d", time.Now().Unix()),
					InitTimeout:   common.Duration(10 * time.Second),
					GetTimeout:    common.Duration(5 * time.Second),
					SetTimeout:    common.Duration(5 * time.Second),
					MinConns:      1,
					MaxConns:      5,
				}
				conn, err := NewPostgreSQLConnector(ctx, &lg, "test-pg", cfg)
				require.NoError(t, err)
				require.NotNil(t, conn)

				// Wait for initialization
				time.Sleep(2 * time.Second)
				return conn
			},
		},
		{
			name: "Redis",
			createConnector: func(t *testing.T) Connector {
				if os.Getenv("REDIS_TEST_URI") == "" {
					t.Skip("Skipping Redis test - REDIS_TEST_URI not set")
				}
				lg := log.Logger
				cfg := &common.RedisConnectorConfig{
					URI:               os.Getenv("REDIS_TEST_URI"),
					InitTimeout:       common.Duration(10 * time.Second),
					GetTimeout:        common.Duration(5 * time.Second),
					SetTimeout:        common.Duration(5 * time.Second),
					LockRetryInterval: common.Duration(100 * time.Millisecond),
				}
				conn, err := NewRedisConnector(ctx, &lg, "test-redis", cfg)
				require.NoError(t, err)
				require.NotNil(t, conn)

				// Wait for initialization
				time.Sleep(2 * time.Second)
				return conn
			},
		},
		{
			name: "Memory",
			createConnector: func(t *testing.T) Connector {
				lg := log.Logger
				cfg := &common.MemoryConnectorConfig{
					MaxItems:     1000,
					MaxTotalSize: "10MB",
				}
				conn, err := NewMemoryConnector(ctx, &lg, "test-memory", cfg)
				require.NoError(t, err)
				require.NotNil(t, conn)
				return conn
			},
			skipReverseIndex: true, // Memory connector doesn't have true reverse index support
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			conn := tc.createConnector(t)

			// Test 1: Direct Get with expired TTL
			t.Run("DirectGet_ExpiredTTL", func(t *testing.T) {
				pk := fmt.Sprintf("test:ttl:direct:%d", time.Now().UnixNano())
				rk := "request-hash-1"
				value := []byte("test-value-1")
				ttl := 1 * time.Second

				// Set with short TTL
				err := conn.Set(ctx, pk, rk, value, &ttl)
				require.NoError(t, err)

				// Ristretto Set is eventually consistent; ensure visibility for memory connector
				if mc, ok := conn.(*MemoryConnector); ok {
					mc.cache.Wait()
				}

				// Immediately get - should succeed (after ensuring visibility for memory)
				result, err := conn.Get(ctx, ConnectorMainIndex, pk, rk, nil)
				assert.NoError(t, err)
				assert.Equal(t, value, result)

				// Wait for expiry
				time.Sleep(2 * time.Second)

				// Get after expiry - should fail
				result, err = conn.Get(ctx, ConnectorMainIndex, pk, rk, nil)
				assert.Error(t, err)
				assert.True(t, common.HasErrorCode(err, common.ErrCodeRecordNotFound) ||
					common.HasErrorCode(err, common.ErrCodeRecordExpired))
				assert.Nil(t, result)
			})

			// Test 2: Reverse index Get with expired TTL (realtime cache scenario)
			if !tc.skipReverseIndex {
				t.Run("ReverseIndexGet_ExpiredTTL", func(t *testing.T) {
					// Simulate a realtime cache entry
					networkId := "evm:1"
					pk := fmt.Sprintf("%s:*", networkId)           // Wildcard partition key for realtime
					actualPk := fmt.Sprintf("%s:12345", networkId) // Actual stored key
					rk := fmt.Sprintf("request-hash-%d", time.Now().UnixNano())
					value := []byte("realtime-value")
					ttl := 1 * time.Second

					// Set with short TTL (simulating cache write)
					err := conn.Set(ctx, actualPk, rk, value, &ttl)
					require.NoError(t, err)

					// Immediately get through reverse index - should succeed
					result, err := conn.Get(ctx, ConnectorReverseIndex, pk, rk, nil)
					assert.NoError(t, err)
					assert.Equal(t, value, result)

					// Wait for expiry
					time.Sleep(2 * time.Second)

					// Get through reverse index after expiry - should fail
					result, err = conn.Get(ctx, ConnectorReverseIndex, pk, rk, nil)
					assert.Error(t, err)
					assert.True(t, common.HasErrorCode(err, common.ErrCodeRecordNotFound) ||
						common.HasErrorCode(err, common.ErrCodeRecordExpired))
					assert.Nil(t, result)
				})
			}

			// Test 3: Wildcard Get with expired TTL (PostgreSQL specific)
			if tc.name == "PostgreSQL" {
				t.Run("WildcardGet_ExpiredTTL", func(t *testing.T) {
					pk := fmt.Sprintf("test:wildcard:%d", time.Now().UnixNano())
					rk := "request-*" // Wildcard range key
					actualRk := "request-123"
					value := []byte("wildcard-value")
					ttl := 1 * time.Second

					// Set with short TTL
					err := conn.Set(ctx, pk, actualRk, value, &ttl)
					require.NoError(t, err)

					// Immediately get with wildcard - should succeed
					result, err := conn.Get(ctx, ConnectorMainIndex, pk, rk, nil)
					assert.NoError(t, err)
					assert.Equal(t, value, result)

					// Wait for expiry
					time.Sleep(2 * time.Second)

					// Get with wildcard after expiry - should fail
					result, err = conn.Get(ctx, ConnectorMainIndex, pk, rk, nil)
					assert.Error(t, err)
					assert.True(t, common.HasErrorCode(err, common.ErrCodeRecordNotFound))
					assert.Nil(t, result)
				})
			}

			// Test 4: Multiple entries with different TTLs
			if !tc.skipReverseIndex {
				t.Run("MultipleEntries_DifferentTTLs", func(t *testing.T) {
					networkId := "evm:1"
					pk1 := fmt.Sprintf("%s:block1", networkId)
					pk2 := fmt.Sprintf("%s:block2", networkId)
					wildcardPk := fmt.Sprintf("%s:*", networkId)
					rk := fmt.Sprintf("multi-hash-%d", time.Now().UnixNano())
					value1 := []byte("value-1")
					value2 := []byte("value-2")
					ttl1 := 1 * time.Second
					ttl2 := 5 * time.Second

					// Set two entries with different TTLs
					err := conn.Set(ctx, pk1, rk, value1, &ttl1)
					require.NoError(t, err)
					err = conn.Set(ctx, pk2, rk, value2, &ttl2)
					require.NoError(t, err)

					// Get through reverse index - should get one of them
					result, err := conn.Get(ctx, ConnectorReverseIndex, wildcardPk, rk, nil)
					assert.NoError(t, err)
					assert.True(t, string(result) == string(value1) || string(result) == string(value2))

					// Wait for first TTL to expire
					time.Sleep(2 * time.Second)

					// Get through reverse index - should only get the second one
					result, err = conn.Get(ctx, ConnectorReverseIndex, wildcardPk, rk, nil)
					if tc.name == "Redis" {
						// Redis might still return the first one's reference in reverse index
						// but the actual key lookup will fail, resulting in overall failure
						if err != nil {
							assert.True(t, common.HasErrorCode(err, common.ErrCodeRecordNotFound))
						} else {
							assert.Equal(t, value2, result)
						}
					} else {
						assert.NoError(t, err)
						assert.Equal(t, value2, result)
					}
				})
			}

			// Clean up for connectors that support it
			if tc.name == "PostgreSQL" || tc.name == "DynamoDB" {
				// Tables will be cleaned up by database TTL or test cleanup
				// For production, you'd want to drop/delete tables here
			}
		})
	}
}
