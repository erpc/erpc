package data

import (
	"fmt"
	"strings"
	"sync"

	"github.com/flair-sdk/erpc/config"
)

type MemoryStore struct {
	Store
	sync.RWMutex
	config *config.MemoryStore
	data   map[string][]byte
}

func NewMemoryStore(cfg *config.MemoryStore) *MemoryStore {
	return &MemoryStore{
		config: cfg,
		data:   make(map[string][]byte),
	}
}

func (m *MemoryStore) Get(key string) ([]byte, error) {
	m.RLock()
	defer m.RUnlock()
	value, ok := m.data[key]
	if !ok {
		return nil, fmt.Errorf("key not found: %s", key)
	}
	return value, nil
}

func (m *MemoryStore) Set(key string, value []byte) (int, error) {
	m.Lock()
	defer m.Unlock()
	m.data[key] = value
	return len(value), nil
}

func (m *MemoryStore) Scan(prefix string) ([][]byte, error) {
	m.RLock()
	defer m.RUnlock()
	var values [][]byte
	for key, value := range m.data {
		if strings.HasPrefix(key, prefix) {
			values = append(values, value)
		}
	}
	return values, nil
}

func (m *MemoryStore) Delete(key string) error {
	m.Lock()
	defer m.Unlock()
	if _, ok := m.data[key]; !ok {
		return fmt.Errorf("key not found: %s", key)
	}
	delete(m.data, key)
	return nil
}
