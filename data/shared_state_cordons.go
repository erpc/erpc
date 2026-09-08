package data

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/erpc/erpc/common"
)

// CordonStore is the source of truth for a project's operator cordons. Every
// write returns the resulting snapshot so the caller applies exactly what was
// persisted; readers poll Get and keep the newest version they have seen.
type CordonStore interface {
	Get(ctx context.Context, projectId string) (*common.CordonSnapshot, error)
	Set(ctx context.Context, projectId, upstreamId, method, reason string) (*common.CordonSnapshot, error)
	Delete(ctx context.Context, projectId, upstreamId, method string) (*common.CordonSnapshot, error)
}

// connectorCordonStore keeps one record per project in the shared-state
// connector. Writes are read-modify-write under the connector's distributed
// lock; the record is never deleted so its version stays monotonic.
type connectorCordonStore struct {
	registry *sharedStateRegistry
}

const cordonRangeKey = "operator"

func (s *connectorCordonStore) key(projectId string) string {
	return fmt.Sprintf("%s/cordons/%s", s.registry.clusterKey, projectId)
}

func (s *connectorCordonStore) Get(ctx context.Context, projectId string) (*common.CordonSnapshot, error) {
	raw, err := s.registry.connector.Get(ctx, ConnectorMainIndex, s.key(projectId), cordonRangeKey, nil)
	if err != nil {
		if common.HasErrorCode(err, common.ErrCodeRecordNotFound) {
			return &common.CordonSnapshot{Cordons: common.ProjectCordons{}}, nil
		}
		return nil, err
	}
	var snap common.CordonSnapshot
	if err := common.SonicCfg.Unmarshal(raw, &snap); err != nil {
		return nil, fmt.Errorf("cordons record unmarshal failed: %w", err)
	}
	if snap.Cordons == nil {
		snap.Cordons = common.ProjectCordons{}
	}
	return &snap, nil
}

func (s *connectorCordonStore) Set(ctx context.Context, projectId, upstreamId, method, reason string) (*common.CordonSnapshot, error) {
	return s.update(ctx, projectId, func(c common.ProjectCordons) common.ProjectCordons {
		return c.With(upstreamId, method, common.CordonEntry{Reason: reason}, time.Now().UnixMilli())
	})
}

func (s *connectorCordonStore) Delete(ctx context.Context, projectId, upstreamId, method string) (*common.CordonSnapshot, error) {
	return s.update(ctx, projectId, func(c common.ProjectCordons) common.ProjectCordons {
		return c.Without(upstreamId, method)
	})
}

func (s *connectorCordonStore) update(ctx context.Context, projectId string, mutate func(common.ProjectCordons) common.ProjectCordons) (*common.CordonSnapshot, error) {
	r := s.registry
	pk := s.key(projectId)
	lock, err := r.connector.Lock(ctx, pk, r.lockTtl)
	if err != nil {
		return nil, fmt.Errorf("failed to acquire cordons lock: %w", err)
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(r.appCtx, r.lockTtl)
		defer cancel()
		if err := lock.Unlock(unlockCtx); err != nil {
			r.logger.Debug().Err(err).Str("key", pk).Msg("failed to unlock cordons record; lock expires after ttl")
		}
	}()

	cur, err := s.Get(ctx, projectId)
	if err != nil {
		return nil, fmt.Errorf("failed to read cordons record: %w", err)
	}
	next := &common.CordonSnapshot{Version: cur.Version + 1, Cordons: mutate(cur.Cordons)}
	payload, err := common.SonicCfg.Marshal(next)
	if err != nil {
		return nil, err
	}
	if err := r.connector.Set(ctx, pk, cordonRangeKey, payload, nil); err != nil {
		return nil, fmt.Errorf("failed to write cordons record: %w", err)
	}
	return next, nil
}

// memoryCordonStore is the process-local store used when shared state has no
// remote connector: same contract, nothing to share.
type memoryCordonStore struct {
	mu       sync.Mutex
	projects map[string]*common.CordonSnapshot
}

func NewMemoryCordonStore() CordonStore {
	return &memoryCordonStore{projects: map[string]*common.CordonSnapshot{}}
}

func (s *memoryCordonStore) Get(_ context.Context, projectId string) (*common.CordonSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if snap, ok := s.projects[projectId]; ok {
		return snap, nil
	}
	return &common.CordonSnapshot{Cordons: common.ProjectCordons{}}, nil
}

func (s *memoryCordonStore) Set(_ context.Context, projectId, upstreamId, method, reason string) (*common.CordonSnapshot, error) {
	return s.update(projectId, func(c common.ProjectCordons) common.ProjectCordons {
		return c.With(upstreamId, method, common.CordonEntry{Reason: reason}, time.Now().UnixMilli())
	}), nil
}

func (s *memoryCordonStore) Delete(_ context.Context, projectId, upstreamId, method string) (*common.CordonSnapshot, error) {
	return s.update(projectId, func(c common.ProjectCordons) common.ProjectCordons {
		return c.Without(upstreamId, method)
	}), nil
}

func (s *memoryCordonStore) update(projectId string, mutate func(common.ProjectCordons) common.ProjectCordons) *common.CordonSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	cur, ok := s.projects[projectId]
	if !ok {
		cur = &common.CordonSnapshot{Cordons: common.ProjectCordons{}}
	}
	next := &common.CordonSnapshot{Version: cur.Version + 1, Cordons: mutate(cur.Cordons)}
	s.projects[projectId] = next
	return next
}
