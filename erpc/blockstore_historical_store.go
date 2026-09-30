package erpc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/erpc/erpc/blockstore"
	"github.com/erpc/erpc/data"
)

type blockStoreHistoricalStore struct {
	connector data.Connector
}

var _ blockstore.HistoricalStore = (*blockStoreHistoricalStore)(nil)

func (s *blockStoreHistoricalStore) historicalPartition(scope blockstore.Scope) (string, error) {
	return (&blockStoreConnectorStore{}).partition(scope)
}

func (s *blockStoreHistoricalStore) GetHistoricalBlock(ctx context.Context, scope blockstore.Scope, key string) (json.RawMessage, error) {
	return s.getHistoricalPayload(ctx, scope, "block", key)
}
func (s *blockStoreHistoricalStore) PutHistoricalBlock(ctx context.Context, scope blockstore.Scope, key string, payload json.RawMessage, ttl time.Duration) error {
	return s.putHistoricalPayload(ctx, scope, "block", key, payload, ttl)
}
func (s *blockStoreHistoricalStore) GetHistoricalLogs(ctx context.Context, scope blockstore.Scope, key string) (json.RawMessage, json.RawMessage, error) {
	payload, err := s.getHistoricalPayload(ctx, scope, "logs", key)
	if err != nil {
		return nil, nil, err
	}
	var pair struct {
		Header json.RawMessage `json:"header"`
		Logs   json.RawMessage `json:"logs"`
	}
	if err := json.Unmarshal(payload, &pair); err != nil {
		return nil, nil, fmt.Errorf("decode historical logs: %w", err)
	}
	if !json.Valid(pair.Header) || !json.Valid(pair.Logs) {
		return nil, nil, fmt.Errorf("historical logs payload mismatch")
	}
	return pair.Header, pair.Logs, nil
}
func (s *blockStoreHistoricalStore) PutHistoricalLogs(ctx context.Context, scope blockstore.Scope, key string, header, logs json.RawMessage, ttl time.Duration) error {
	if !json.Valid(header) || !json.Valid(logs) {
		return fmt.Errorf("invalid historical logs payload")
	}
	payload, err := json.Marshal(struct {
		Header json.RawMessage `json:"header"`
		Logs   json.RawMessage `json:"logs"`
	}{header, logs})
	if err != nil {
		return fmt.Errorf("encode historical logs: %w", err)
	}
	return s.putHistoricalPayload(ctx, scope, "logs", key, payload, ttl)
}

func (s *blockStoreHistoricalStore) getHistoricalPayload(ctx context.Context, scope blockstore.Scope, kind, key string) (json.RawMessage, error) {
	partition, err := s.historicalPartition(scope)
	if err != nil {
		return nil, err
	}
	value, err := s.connector.Get(ctx, data.ConnectorMainIndex, partition, historicalKey(kind, key), nil)
	if err != nil {
		return nil, err
	}
	if len(value) == 0 {
		return nil, blockstore.ErrNotFound
	}
	var envelope struct {
		Kind    string          `json:"kind"`
		Key     string          `json:"key"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(value, &envelope); err != nil {
		return nil, fmt.Errorf("decode historical %s payload: %w", kind, err)
	}
	if envelope.Kind != kind || envelope.Key != key || len(envelope.Payload) == 0 || !json.Valid(envelope.Payload) {
		return nil, fmt.Errorf("historical %s payload index mismatch", kind)
	}
	return envelope.Payload, nil
}
func (s *blockStoreHistoricalStore) putHistoricalPayload(ctx context.Context, scope blockstore.Scope, kind, key string, payload json.RawMessage, ttl time.Duration) error {
	if len(payload) == 0 || !json.Valid(payload) {
		return fmt.Errorf("invalid historical %s payload", kind)
	}
	envelope, err := json.Marshal(struct {
		Kind    string          `json:"kind"`
		Key     string          `json:"key"`
		Payload json.RawMessage `json:"payload"`
	}{kind, key, payload})
	if err != nil {
		return fmt.Errorf("encode historical %s payload: %w", kind, err)
	}
	return s.putHistoricalEncoded(ctx, scope, kind, key, envelope, ttl)
}
func (s *blockStoreHistoricalStore) putHistoricalEncoded(ctx context.Context, scope blockstore.Scope, kind, key string, payload []byte, ttl time.Duration) error {
	if ttl <= 0 {
		return fmt.Errorf("historical %s TTL must be positive", kind)
	}
	partition, err := s.historicalPartition(scope)
	if err != nil {
		return err
	}
	return s.connector.Set(ctx, partition, historicalKey(kind, key), payload, &ttl)
}

func (s *blockStoreHistoricalStore) GetFinalizedHash(ctx context.Context, scope blockstore.Scope, height int64) (string, error) {
	if height < 0 {
		return "", fmt.Errorf("historical finalized height must be nonnegative")
	}
	partition, err := s.historicalPartition(scope)
	if err != nil {
		return "", err
	}
	value, err := s.connector.Get(ctx, data.ConnectorMainIndex, partition, historicalKey("finalized", fmt.Sprint(height)), nil)
	if err != nil {
		return "", err
	}
	if len(value) == 0 {
		return "", blockstore.ErrNotFound
	}
	var payload struct {
		Kind   string `json:"kind"`
		Height int64  `json:"height"`
		Hash   string `json:"hash"`
	}
	if err := json.Unmarshal(value, &payload); err != nil {
		return "", fmt.Errorf("decode historical finalized index: %w", err)
	}
	if payload.Kind != "finalized" || payload.Height != height || payload.Hash == "" {
		return "", fmt.Errorf("historical finalized index payload mismatch")
	}
	return payload.Hash, nil
}
func (s *blockStoreHistoricalStore) PutFinalizedHash(ctx context.Context, scope blockstore.Scope, height int64, hash string, ttl time.Duration) error {
	if height < 0 || hash == "" {
		return fmt.Errorf("invalid historical finalized index")
	}
	value, err := json.Marshal(struct {
		Kind   string `json:"kind"`
		Height int64  `json:"height"`
		Hash   string `json:"hash"`
	}{"finalized", height, hash})
	if err != nil {
		return fmt.Errorf("encode historical finalized index: %w", err)
	}
	partition, err := s.historicalPartition(scope)
	if err != nil {
		return err
	}
	if ttl <= 0 {
		return fmt.Errorf("historical finalized index TTL must be positive")
	}
	return s.connector.Set(ctx, partition, historicalKey("finalized", fmt.Sprint(height)), value, &ttl)
}

func historicalKey(kind, key string) string {
	h := sha256.Sum256([]byte(key))
	return kind + ":" + hex.EncodeToString(h[:])
}
