package data

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/config"
)

const (
	MemoryConnectorDriver = "memory"
)

type MemoryStore struct {
	sync.RWMutex
	config *config.MemoryConnectorConfig
	data   map[string]string
}

func NewMemoryStore(cfg *config.MemoryConnectorConfig) *MemoryStore {
	return &MemoryStore{
		config: cfg,
		data:   make(map[string]string),
	}
}

func (m *MemoryStore) Get(ctx context.Context, key string) (string, error) {
	m.RLock()
	defer m.RUnlock()
	value, ok := m.data[key]
	if !ok {
		return "", common.NewErrRecordNotFound(key, MemoryConnectorDriver)
	}
	return value, nil
}

func (r *MemoryStore) GetWithReader(ctx context.Context, key string) (io.Reader, error) {
	value, err := r.Get(ctx, key)
	if err != nil {
		return nil, err
	}

	return strings.NewReader(value), nil
}

func (m *MemoryStore) Set(ctx context.Context, key string, value string) (int, error) {
	m.Lock()
	defer m.Unlock()
	m.data[key] = value
	return len(value), nil
}

func (m *MemoryStore) SetWithWriter(ctx context.Context, key string) (io.WriteCloser, error) {
	m.Lock()
	defer m.Unlock()
	delete(m.data, key)
	return &MemoryValueWriter{memoryStore: m, key: key}, nil
}

func (m *MemoryStore) Scan(ctx context.Context, prefix string) ([]string, error) {
	m.RLock()
	defer m.RUnlock()
	var values []string
	for key, value := range m.data {
		if strings.HasPrefix(key, prefix) {
			values = append(values, value)
		}
	}
	return values, nil
}

func (m *MemoryStore) Delete(ctx context.Context, key string) error {
	m.Lock()
	defer m.Unlock()
	if _, ok := m.data[key]; !ok {
		return fmt.Errorf("key not found: %s", key)
	}
	delete(m.data, key)
	return nil
}

type MemoryValueWriter struct {
	memoryStore *MemoryStore
	key         string
	buffer      strings.Builder
}

func (w *MemoryValueWriter) Write(p []byte) (n int, err error) {
	w.buffer.Write(p)
	return len(p), nil
}

func (w *MemoryValueWriter) Close() error {
	w.memoryStore.Lock()
	defer w.memoryStore.Unlock()
	w.memoryStore.data[w.key] = w.buffer.String()
	return nil
}
