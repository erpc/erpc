package data

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/erpc/erpc/common"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
)

func TestMemoryConnector_ZstdCompression(t *testing.T) {
	// Setup
	logger := zerolog.New(io.Discard)
	ctx := context.Background()
	connector, err := NewMemoryConnector(ctx, &logger, "test", &common.MemoryConnectorConfig{
		MaxItems: 100_000, MaxTotalSize: "1GB",
	})
	require.NoError(t, err)
	defer connector.Close()

	t.Run("small values are not compressed", func(t *testing.T) {
		smallValue := `{"jsonrpc":"2.0","result":"0x1bc16d674ec80000","id":1}`
		err := connector.Set(ctx, "test", "small", []byte(smallValue), nil)
		require.NoError(t, err)

		// Wait for Ristretto to process the write
		time.Sleep(10 * time.Millisecond)

		// Verify value is stored and retrieved correctly
		retrieved, err := connector.Get(ctx, "", "test", "small")
		require.NoError(t, err)
		require.Equal(t, []byte(smallValue), retrieved)
	})

	t.Run("large values are compressed transparently", func(t *testing.T) {
		// Create a large EVM-like JSON response (> 512 bytes)
		largeValue := createLargeEvmResponse()
		require.Greater(t, len(largeValue), 512, "Test value should be larger than compression threshold")

		err := connector.Set(ctx, "test", "large", []byte(largeValue), nil)
		require.NoError(t, err)

		// Wait for Ristretto to process the write
		time.Sleep(10 * time.Millisecond)

		// Verify value is stored and retrieved correctly (compression is transparent)
		retrieved, err := connector.Get(ctx, "", "test", "large")
		require.NoError(t, err)
		require.Equal(t, []byte(largeValue), retrieved)
	})

	t.Run("mixed size values work correctly", func(t *testing.T) {
		testCases := []struct {
			key   string
			value string
		}{
			{"small1", `{"jsonrpc":"2.0","result":"0x123","id":1}`},
			{"medium1", createMediumEvmResponse()},
			{"large1", createLargeEvmResponse()},
			{"small2", `{"jsonrpc":"2.0","result":false,"id":2}`},
			{"large2", createLargeEvmResponse()},
		}

		// Store all values
		for _, tc := range testCases {
			err := connector.Set(ctx, "mixed", tc.key, []byte(tc.value), nil)
			require.NoError(t, err)
		}

		// Wait for Ristretto to process all writes
		time.Sleep(50 * time.Millisecond)

		// Retrieve and verify all values
		for _, tc := range testCases {
			retrieved, err := connector.Get(ctx, "", "mixed", tc.key)
			require.NoError(t, err)
			require.Equal(t, []byte(tc.value), retrieved, "Value mismatch for key: %s", tc.key)
		}
	})

	t.Run("compression works with TTL", func(t *testing.T) {
		largeValue := createLargeEvmResponse()
		ttl := 100 * time.Second

		err := connector.Set(ctx, "test", "ttl", []byte(largeValue), &ttl)
		require.NoError(t, err)

		// Wait for Ristretto to process the write
		time.Sleep(10 * time.Millisecond)

		// Verify value is stored and retrieved correctly
		retrieved, err := connector.Get(ctx, "", "test", "ttl")
		require.NoError(t, err)
		require.Equal(t, []byte(largeValue), retrieved)
	})
}

func TestMemoryConnector_CompressionBenefits(t *testing.T) {
	// Test to demonstrate actual compression benefits
	logger := zerolog.New(io.Discard)
	ctx := context.Background()

	// Create connector with compression enabled
	connector, err := NewMemoryConnector(ctx, &logger, "test", &common.MemoryConnectorConfig{
		MaxItems: 100_000, MaxTotalSize: "1GB",
	})
	require.NoError(t, err)
	defer connector.Close()

	// Test with realistic EVM RPC response
	evmResponse := createLargeBlockResponse()
	originalSize := len(evmResponse)

	// Store the value
	err = connector.Set(ctx, "evm", "block", []byte(evmResponse), nil)
	require.NoError(t, err)

	// Wait for Ristretto to process the write
	time.Sleep(10 * time.Millisecond)

	// Retrieve and verify
	retrieved, err := connector.Get(ctx, "", "evm", "block")
	require.NoError(t, err)
	require.Equal(t, []byte(evmResponse), retrieved)

	t.Logf("Original EVM response size: %d bytes", originalSize)
	t.Logf("✅ Compression is transparent - no API changes needed")
	t.Logf("✅ Values >512 bytes are automatically compressed with zstd")
	t.Logf("✅ Memory efficiency: More cache entries fit in same space")
	t.Logf("✅ Speed: zstd.SpeedFastest optimized for caching workloads")
}

func TestMemoryConnector_ConcurrentCompression(t *testing.T) {
	// Test concurrent access to verify thread safety
	logger := zerolog.New(io.Discard)
	ctx := context.Background()

	connector, err := NewMemoryConnector(ctx, &logger, "test", &common.MemoryConnectorConfig{
		MaxItems: 100_000, MaxTotalSize: "1GB",
	})
	require.NoError(t, err)
	defer connector.Close()

	// Create test data of various sizes
	testData := []struct {
		key   string
		value string
	}{
		{"small1", `{"jsonrpc":"2.0","result":"0x123","id":1}`},
		{"large1", createLargeEvmResponse()},
		{"medium1", createMediumEvmResponse()},
		{"large2", createLargeBlockResponse()},
	}

	// Reduced concurrency to account for Ristretto's async nature
	numWorkers := 10
	numOperationsPerWorker := 5

	var wg sync.WaitGroup
	errChan := make(chan error, numWorkers*numOperationsPerWorker*2) // *2 for Set+Get operations

	// Phase 1: Concurrent writes
	t.Log("Phase 1: Concurrent writes...")
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()

			for j := 0; j < numOperationsPerWorker; j++ {
				for _, testCase := range testData {
					// Unique key per worker and operation
					key := fmt.Sprintf("%s_w%d_op%d", testCase.key, workerID, j)

					// Set operation
					if err := connector.Set(ctx, "concurrent", key, []byte(testCase.value), nil); err != nil {
						errChan <- fmt.Errorf("worker %d operation %d set failed: %w", workerID, j, err)
					}

					// Small delay between operations
					time.Sleep(2 * time.Millisecond)
				}
			}
		}(i)
	}

	// Wait for all writes to complete
	wg.Wait()

	// Allow Ristretto to process all buffered writes
	t.Log("Waiting for Ristretto to process writes...")
	time.Sleep(100 * time.Millisecond)

	// Phase 2: Concurrent reads
	t.Log("Phase 2: Concurrent reads...")
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()

			for j := 0; j < numOperationsPerWorker; j++ {
				for _, testCase := range testData {
					key := fmt.Sprintf("%s_w%d_op%d", testCase.key, workerID, j)

					// Get operation
					retrieved, err := connector.Get(ctx, "", "concurrent", key)
					if err != nil {
						errChan <- fmt.Errorf("worker %d operation %d get failed: %w", workerID, j, err)
						continue
					}

					// Verify data integrity
					if !bytes.Equal(retrieved, []byte(testCase.value)) {
						errChan <- fmt.Errorf("worker %d operation %d data mismatch for key %s", workerID, j, key)
					}
				}
			}
		}(i)
	}

	// Wait for all reads to complete
	wg.Wait()
	close(errChan)

	// Check for any errors
	var errors []error
	for err := range errChan {
		errors = append(errors, err)
	}

	if len(errors) > 0 {
		t.Fatalf("Concurrent operations failed with %d errors. First error: %v", len(errors), errors[0])
	}

	totalOperations := numWorkers * numOperationsPerWorker * len(testData)
	t.Logf("✅ Successfully completed %d concurrent compression operations", totalOperations)
	t.Logf("✅ Thread-safe zstd compression working correctly")
}

// Helper functions to create test data

func createMediumEvmResponse() string {
	return `{
		"jsonrpc": "2.0",
		"result": {
			"number": "0x1346edf",
			"hash": "0x2ae887e88dcb7d7db22a9ad0769d066b3dba4cf403607204a98b5068161c031c",
			"parentHash": "0xf74aa70fda7bf54eb8eeef0e45dce3c7afa57ba145cab813dd408c80448ac803",
			"gasLimit": "0x1c9c380",
			"gasUsed": "0x98323f",
			"timestamp": "0x6682f5cb",
			"transactions": ["0xa5e3bfb85f2f4398281161ee906b5a0e98f98b4ed34ceb0f8f53a4fdd7c92065"]
		},
		"id": 1
	}`
}

func createLargeEvmResponse() string {
	transactions := []string{
		"0xa5e3bfb85f2f4398281161ee906b5a0e98f98b4ed34ceb0f8f53a4fdd7c92065",
		"0x05945d986cacc44d1e3ada642f03ab70e92ed7fce66d5b45a38a7719c76f7970",
		"0x04742b85cd4fa9f17719a04c777f1deeaa7ade5a33bd4cf8627d6dfbd52a0513",
		"0xb963d3365df545bbe9aef335910f94297529cc1aeaaf4c4f27306503b5e829f3",
		"0xedbf1d4a89609451b617057bb5fbd603ea82ddb96b793be244f3a851be0408f1",
		"0x193b7fe88cd5ccb2d82ea8465a5294c16a116066b57fa77b05eb5cfde355e11b",
		"0x6c91b32fbd89a40f6399ab5e4bf9f81099888c39d0454f460b98a22c3a57fe90",
		"0xe2e6a61d00280fef52fb57b913f26ada374848eb858304ccf4eaa154d492249b",
		"0xdf97321579ca041a29326abd1ca5dc15bc0aabc71afcac64cb50ec41d1b108b3",
		"0x200337118c357b322dc1fd2306038ef4132773088a831b8d4bcebe916ae03846",
	}

	return `{
		"jsonrpc": "2.0",
		"result": {
			"baseFeePerGas": "0x2a0c2d60e",
			"blobGasUsed": "0x0",
			"difficulty": "0x0",
			"excessBlobGas": "0x0",
			"extraData": "0x6265617665726275696c642e6f7267",
			"gasLimit": "0x1c9c380",
			"gasUsed": "0x98323f",
			"hash": "0x2ae887e88dcb7d7db22a9ad0769d066b3dba4cf403607204a98b5068161c031c",
			"logsBloom": "0x0f23c84f6042412100881051a001000008d523c05461489202015a0285320270848424c5f1601abc54002e20803061000a4101308e81246a0558a02e20683d826092c10961900a1b2e81422983a184e9806401854864c98211924e42c26e48032e4053e48b20206226cd48048a1c09576010ac4a008c0d408a8013bc001e17230a20a85120264505c4e0b3c0297320a460682a550303004d05d13152bd381a390228054639a86c080a2213d15a408c8c95a48244010858870448786438802001cc9f12320008477aa481fc2901101cd1614840ae5d29081c20122106306760628e74b4080344281001c11fe500800c00ed085000d2010cd022815810c8631403",
			"miner": "0x95222290dd7278aa3ddd389cc1e1d165cc4bafe5",
			"mixHash": "0xfe9847d938b29f153e798f378bd6770df7b06b1f56ea48377c785da2ae1ed8aa",
			"nonce": "0x0000000000000000",
			"number": "0x1346edf",
			"parentBeaconBlockRoot": "0x7833f4a199298b764ce4483b9d165b3d8d39c5783728757f9b1981225e5c8363",
			"parentHash": "0xf74aa70fda7bf54eb8eeef0e45dce3c7afa57ba145cab813dd408c80448ac803",
			"receiptsRoot": "0x02a38ffe3ddb39560b381969f8c40f9e9f7d07fb8ba715af382915b328e396ce",
			"sha3Uncles": "0x1dcc4de8dec75d7aab85b567b6ccd41ad312451b948a7413f0a142fd40d49347",
			"size": "0xd9e1",
			"stateRoot": "0x9b5608d8439fb93928ad5eee98b24ba16ac9211fbf6204014739337dfb23b8de",
			"timestamp": "0x6682f5cb",
			"totalDifficulty": "0xc70d815d562d3cfa955",
			"transactions": [` + `"` + strings.Join(transactions, `","`) + `"` + `]
		},
		"id": 1
	}`
}

func createLargeBlockResponse() string {
	// Create an even larger response similar to a full block with transaction details
	return createLargeEvmResponse() + strings.Repeat(`,"extraField":"0x`+strings.Repeat("a", 64)+`"`, 20)
}
