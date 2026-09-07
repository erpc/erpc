package data

import (
	"context"
	"fmt"

	"github.com/erpc/erpc/common"
)

// CordonStore persists operator cordons for a project so every replica sees
// the same set and a restarted pod restores it. One record per project holds
// upstreamId → method → entry; writes are read-modify-write under the
// connector's distributed lock. Readers poll (see upstream.UpstreamsRegistry).
type CordonStore interface {
	Get(ctx context.Context, projectId string) (ProjectCordons, error)
	Set(ctx context.Context, projectId, upstreamId, method string, entry common.CordonEntry) error
	Delete(ctx context.Context, projectId, upstreamId, method string) error
}

// ProjectCordons is upstreamId → method → entry.
type ProjectCordons map[string]map[string]common.CordonEntry

type cordonStore struct {
	registry *sharedStateRegistry
}

const cordonRangeKey = "admin"

func (s *cordonStore) key(projectId string) string {
	return fmt.Sprintf("%s/cordons/%s", s.registry.clusterKey, projectId)
}

func (s *cordonStore) Get(ctx context.Context, projectId string) (ProjectCordons, error) {
	raw, err := s.registry.connector.Get(ctx, ConnectorMainIndex, s.key(projectId), cordonRangeKey, nil)
	if err != nil {
		if common.HasErrorCode(err, common.ErrCodeRecordNotFound) {
			return ProjectCordons{}, nil
		}
		return nil, err
	}
	var m ProjectCordons
	if err := common.SonicCfg.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("cordons record unmarshal failed: %w", err)
	}
	if m == nil {
		m = ProjectCordons{}
	}
	return m, nil
}

func (s *cordonStore) Set(ctx context.Context, projectId, upstreamId, method string, entry common.CordonEntry) error {
	return s.update(ctx, projectId, func(m ProjectCordons) {
		if m[upstreamId] == nil {
			m[upstreamId] = map[string]common.CordonEntry{}
		}
		m[upstreamId][method] = entry
	})
}

func (s *cordonStore) Delete(ctx context.Context, projectId, upstreamId, method string) error {
	return s.update(ctx, projectId, func(m ProjectCordons) {
		delete(m[upstreamId], method)
		if len(m[upstreamId]) == 0 {
			delete(m, upstreamId)
		}
	})
}

func (s *cordonStore) update(ctx context.Context, projectId string, mutate func(ProjectCordons)) error {
	r := s.registry
	pk := s.key(projectId)
	lock, err := r.connector.Lock(ctx, pk, r.lockTtl)
	if err != nil {
		return fmt.Errorf("failed to acquire cordons lock: %w", err)
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(r.appCtx, r.lockTtl)
		defer cancel()
		if err := lock.Unlock(unlockCtx); err != nil {
			r.logger.Debug().Err(err).Str("key", pk).Msg("failed to unlock cordons record; lock expires after ttl")
		}
	}()

	m, err := s.Get(ctx, projectId)
	if err != nil {
		return fmt.Errorf("failed to read cordons record: %w", err)
	}
	mutate(m)
	if len(m) == 0 {
		if err := r.connector.Delete(ctx, pk, cordonRangeKey); err != nil && !common.HasErrorCode(err, common.ErrCodeRecordNotFound) {
			return fmt.Errorf("failed to delete cordons record: %w", err)
		}
		return nil
	}
	payload, err := common.SonicCfg.Marshal(m)
	if err != nil {
		return err
	}
	if err := r.connector.Set(ctx, pk, cordonRangeKey, payload, nil); err != nil {
		return fmt.Errorf("failed to write cordons record: %w", err)
	}
	return nil
}
