package erpc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/erpc/erpc/blockstore"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/data"
	"github.com/redis/go-redis/v9"
)

type blockStoreBypassKey struct{}

// blockStoreDirected reports whether a request's directives take it outside
// the default trust set the block store was filled from:
// internal requests, a specific upstream, an integrity selector, or any
// skipCacheRead other than an explicit "false". Unlike
// ShouldSkipCacheRead(""), connector-ID patterns also bypass, because these
// stores sit in front of every connector; "false" matches its no-skip meaning.
func blockStoreDirected(d *common.RequestDirectives) bool {
	if d == nil {
		return false
	}
	skip := d.SkipCacheRead != "" && !strings.EqualFold(d.SkipCacheRead, "false")
	return d.IsInternal || d.UseUpstream != "" || d.IntegritySelector != "" || skip
}

// exceedsGetLogsLimits reports whether the network's eth_getLogs hard limits
// (getLogsMaxAllowedRange/Addresses/Topics) would reject this filter. Counting
// mirrors networkPreForward_eth_getLogs (architecture/evm/eth_getLogs.go):
// addresses only when given as an array, topics as the topic0 OR-list length
// (or 1 for a single topic0). A local cache must never answer what the
// network would reject, so callers fall through to the normal path, which
// returns the configured error.
func (n *Network) exceedsGetLogsLimits(filter map[string]interface{}, from, to int64) bool {
	if n.cfg == nil || n.cfg.Evm == nil {
		return false
	}
	evm := n.cfg.Evm
	if lim := evm.GetLogsMaxAllowedRange; lim > 0 && to >= from && to-from+1 > lim {
		return true
	}
	if lim := evm.GetLogsMaxAllowedAddresses; lim > 0 {
		if addrs, ok := filter["address"].([]interface{}); ok && int64(len(addrs)) > lim {
			return true
		}
	}
	if lim := evm.GetLogsMaxAllowedTopics; lim > 0 {
		if tps, ok := filter["topics"].([]interface{}); ok && len(tps) > 0 {
			count := int64(0)
			if t0, ok := tps[0].([]interface{}); ok {
				count = int64(len(t0))
			} else if tps[0] != nil {
				count = 1
			}
			if count > lim {
				return true
			}
		}
	}
	return false
}

// withBlockStoreBypass marks the block store's own fill fetch: its Forward is
// never served from the block store, never multiplexed with client requests,
// and never written to the ordinary JSON-RPC cache.
func withBlockStoreBypass(ctx context.Context) context.Context {
	return context.WithValue(ctx, blockStoreBypassKey{}, true)
}

// blockStoreFingerprint hashes the complete configured hydration trust set.
// Alias types deliberately bypass the display serializers' redaction: secrets
// affect trust identity but only the digest ever leaves this function.
func blockStoreFingerprint(prj *common.ProjectConfig, nw *common.NetworkConfig) (string, error) {
	type upstreamTrust common.UpstreamConfig
	type providerTrust common.ProviderConfig
	var upstreams, providers []string
	for _, u := range prj.Upstreams {
		if u == nil {
			continue
		}
		b, err := json.Marshal((*upstreamTrust)(u))
		if err != nil {
			return "", fmt.Errorf("block store upstream fingerprint: %w", err)
		}
		upstreams = append(upstreams, string(b))
	}
	for _, p := range prj.Providers {
		if p == nil {
			continue
		}
		overrides := make(map[string]*upstreamTrust, len(p.Overrides))
		for key, u := range p.Overrides {
			overrides[key] = (*upstreamTrust)(u)
		}
		b, err := json.Marshal(struct {
			*providerTrust
			Overrides map[string]*upstreamTrust `json:"overrides"`
		}{(*providerTrust)(p), overrides})
		if err != nil {
			return "", fmt.Errorf("block store provider fingerprint: %w", err)
		}
		providers = append(providers, string(b))
	}
	sort.Strings(upstreams)
	sort.Strings(providers)
	b, err := json.Marshal(struct {
		NetworkID        string
		Upstreams        []string
		Providers        []string
		UpstreamDefaults *upstreamTrust
		NetworkDefaults  *common.NetworkDefaults
		Integrity        *common.IntegrityConfig
		Network          *common.NetworkConfig
	}{nw.NetworkId(), upstreams, providers, (*upstreamTrust)(prj.UpstreamDefaults), prj.NetworkDefaults, prj.Integrity, nw})
	if err != nil {
		return "", fmt.Errorf("block store trust fingerprint: %w", err)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

// initBlockStore builds the network's block store: the on-demand per-height
// eth_getLogs cache. Nil or disabled config leaves the network unchanged.
func (nr *NetworksRegistry) initBlockStore(network *Network, nwCfg *common.NetworkConfig) error {
	if nwCfg.Evm == nil || nwCfg.Evm.BlockStore == nil || !nwCfg.Evm.BlockStore.Enabled {
		return nil
	}
	hc := nwCfg.Evm.BlockStore
	hc.SetDefaults()
	if err := hc.Validate(); err != nil {
		return err
	}
	ns := hc.Namespace
	if ns == "" {
		ns = "default"
	}
	nr.project.cfgMu.RLock()
	fingerprint, err := blockStoreFingerprint(nr.project.Config, nwCfg)
	nr.project.cfgMu.RUnlock()
	if err != nil {
		return err
	}
	scope := blockstore.Scope{Namespace: ns + ":" + fingerprint, ProjectId: network.projectId, NetworkId: network.networkId}
	return nr.initLogsFill(network, hc, scope)
}

// parseExplicitBlockNumber accepts only hex block numbers; tags resolve
// through the normal path (served-tip semantics may differ from our head).
func parseExplicitBlockNumber(s string) (int64, error) {
	if !strings.HasPrefix(s, "0x") || len(s) < 3 || (len(s) > 3 && s[2] == '0') {
		return 0, fmt.Errorf("not an explicit block number")
	}
	n, err := strconv.ParseUint(s[2:], 16, 63)
	return int64(n), err
}

type blockStoreConnectorStore struct {
	connector data.Connector
	redis     *data.RedisConnector
}

func (s *blockStoreConnectorStore) partition(scope blockstore.Scope) (string, error) {
	identity, err := json.Marshal(struct {
		Namespace string
		ProjectID string
		NetworkID string
	}{scope.Namespace, scope.ProjectId, scope.NetworkId})
	if err != nil {
		return "", fmt.Errorf("marshal block store scope: %w", err)
	}
	h := sha256.Sum256(identity)
	return "blockstore:v1:" + hex.EncodeToString(h[:]), nil
}

func (s *blockStoreConnectorStore) redisClient() (redis.UniversalClient, error) {
	if s.redis == nil {
		return nil, fmt.Errorf("block store Redis connector is unavailable")
	}
	client := s.redis.Client()
	if client == nil {
		return nil, blockstore.ErrStoreUnavailable
	}
	return client, nil
}

// blockStoreStore builds the Redis store on the referenced evmJsonRpcCache
// connector. Keep the configured wrapper for its failsafe behavior, but verify
// that it ultimately uses Redis before sharing entries or taking fill locks.
func (nr *NetworksRegistry) blockStoreStore(hc *common.EvmBlockStoreConfig) (*blockStoreConnectorStore, error) {
	var conn data.Connector
	if nr.evmJsonRpcCache != nil {
		conn = nr.evmJsonRpcCache.Connector(hc.ConnectorId)
	}
	resolved := conn
	for {
		u, ok := resolved.(interface{ Unwrap() data.Connector })
		if !ok {
			break
		}
		resolved = u.Unwrap()
	}
	rc, ok := resolved.(*data.RedisConnector)
	if !ok || rc == nil {
		return nil, fmt.Errorf("evm.blockStore.connectorId %q is not an initialized redis connector in database.evmJsonRpcCache", hc.ConnectorId)
	}
	return &blockStoreConnectorStore{connector: conn, redis: rc}, nil
}
