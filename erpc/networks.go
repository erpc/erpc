package erpc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"strconv"
	"sync"
	"time"

	"github.com/failsafe-go/failsafe-go"
	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/config"
	"github.com/flair-sdk/erpc/data"
	"github.com/flair-sdk/erpc/health"
	"github.com/flair-sdk/erpc/resiliency"
	"github.com/flair-sdk/erpc/upstream"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

type PreparedNetwork struct {
	NetworkId        string
	ProjectId        string
	FailsafePolicies []failsafe.Policy[any]
	Config           *config.NetworkConfig
	Logger           *zerolog.Logger
	Upstreams        []*upstream.PreparedUpstream

	mu *sync.RWMutex

	rateLimitersRegistry *resiliency.RateLimitersRegistry
	failsafeExecutor     failsafe.Executor[interface{}]

	rateLimiterDal data.RateLimitersDAL
	cacheDal       data.CacheDAL

	evmBlockTracker    *EvmBlockTracker
	reorderStopperChan chan bool
}

func (n *PreparedNetwork) Bootstrap(ctx context.Context) error {
	if err := n.resolveNetworkId(ctx); err != nil {
		return err
	}

	if n.Architecture() == upstream.ArchitectureEvm {
		n.evmBlockTracker = NewEvmBlockTracker(n)
		if err := n.evmBlockTracker.Bootstrap(ctx); err != nil {
			return err
		}
	}

	go func(pn *PreparedNetwork) {
		ticker := time.NewTicker(1 * time.Second)
		for {
			select {
			case <-pn.reorderStopperChan:
				ticker.Stop()
				return
			case <-ticker.C:
				upsList := reorderUpstreams(pn.Upstreams)
				pn.mu.Lock()
				pn.Upstreams = upsList
				pn.mu.Unlock()
			}
		}
	}(n)

	return nil
}

func (n *PreparedNetwork) Shutdown() {
	if n.evmBlockTracker != nil {
		n.evmBlockTracker.Shutdown()
	}

	if n.reorderStopperChan != nil {
		n.reorderStopperChan <- true
	}
}

func (n *PreparedNetwork) Architecture() string {
	return n.Config.Architecture
}

func (n *PreparedNetwork) Forward(ctx context.Context, req *common.NormalizedRequest, w common.ResponseWriter) error {
	n.Logger.Debug().Object("req", req).Msgf("forwarding request")

	if n.cacheDal != nil {
		n.Logger.Debug().Msgf("checking cache for request")
		cacheReader, err := n.cacheDal.GetWithReader(ctx, req)
		n.Logger.Debug().Err(err).Msgf("cache response: %v", cacheReader)
		if err != nil {
			n.Logger.Debug().Err(err).Msgf("could not find response in cache")
		}
		if cacheReader != nil {
			if w.TryLock() {
				w.AddHeader("Content-Type", "application/json")
				w.AddHeader("X-ERPC-Network", n.NetworkId)
				w.AddHeader("X-ERPC-Cache", "Hit")
				w, err := io.Copy(w, cacheReader)
				n.Logger.Info().Object("req", req).Int64("written", w).Err(err).Msgf("response served from cache")
				return err
			} else {
				return common.NewErrResponseWriteLock("<cache store>")
			}
		}
	}

	if err := n.acquireRateLimitPermit(req); err != nil {
		return err
	}

	var errorsByUpstream = []error{}

	// Configure the cache writer on the response writer so result can be cached
	go (func() {
		if n.cacheDal != nil {
			cwr, err := n.cacheDal.SetWithWriter(ctx, req)
			if err != nil {
				n.Logger.Warn().Err(err).Msgf("could not create cache response writer")
			} else {
				w.AddBodyWriter(cwr)
			}
		}
	})()

	// Function to prepare and forward the request to an upstream
	tryForward := func(
		u *upstream.PreparedUpstream,
		ctx context.Context,
	) (skipped bool, err error) {
		lg := u.Logger.With().Str("network", n.NetworkId).Logger()
		if u.Score < 0 {
			lg.Debug().Msgf("skipping upstream with negative score %d", u.Score)
			return true, nil
		}

		pr, err := u.PrepareRequest(req)
		lg.Debug().Err(err).Msgf("prepared request: %v", pr)
		if pr == nil && err == nil {
			return true, nil
		}
		if err != nil {
			return false, err
		}

		err = n.forwardToUpstream(u, ctx, pr, w)
		if !common.IsNull(err) {
			return false, err
		}

		lg.Info().Msgf("successfully forward request")
		return false, nil
	}

	if n.FailsafePolicies == nil || len(n.FailsafePolicies) == 0 {
		// Handling via simple loop over upstreams until one responds
		n.mu.RLock()
		var upsList = n.Upstreams
		n.mu.RUnlock()
		for _, u := range upsList {
			if _, err := tryForward(u, ctx); err != nil {
				errorsByUpstream = append(errorsByUpstream, err)
				continue
			}
			return nil
		}

		return common.NewErrUpstreamsExhausted(errorsByUpstream)
	}

	// Handling when FailsafePolicies are defined
	mtx := sync.Mutex{}
	i := 0
	_, execErr := n.failsafeExecutor.WithContext(ctx).GetWithExecution(func(exec failsafe.Execution[interface{}]) (interface{}, error) {
		// We should try all upstreams at least once, but using "i" we make sure
		// across different executions of the failsafe we pick up next upstream vs retrying the same upstream.
		// This mimicks a round-robin behavior, for example when doing hedge or retries.
		// Upstream-level retry is handled by the upstream itself (and its own failsafe policies).
		n.mu.RLock()
		upsList := n.Upstreams
		n.mu.RUnlock()

		ln := len(upsList)
		for count := 0; count < ln; count++ {
			mtx.Lock()
			u := upsList[i]
			n.Logger.Debug().Msgf("executing forward current index: %d", i)
			i++
			if i >= ln {
				i = 0
			}
			mtx.Unlock()
			n.Logger.Debug().Msgf("executing forward to upstream: %s", u.Id)

			skipped, err := tryForward(u, exec.Context())
			if err != nil && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)) && exec.Hedges() > 0 {
				n.Logger.Debug().Err(err).Msgf("discarding hedged request to upstream %s: %v", u.Id, skipped)
				return nil, err
			}

			n.Logger.Debug().Err(err).Msgf("forwarded request to upstream %s skipped: %v err: %v", u.Id, skipped, err)
			if !skipped {
				return nil, err
			} else if err != nil {
				errorsByUpstream = append(errorsByUpstream, err)
				continue
			}
		}

		return nil, common.NewErrUpstreamsExhausted(errorsByUpstream)
	})

	if execErr != nil {
		return resiliency.TranslateFailsafeError(execErr)
	}

	return nil
}

// Send will call the upstream and parses the response, used in internal non-proxy flows such as block tracker
func (n *PreparedNetwork) Send(ctx context.Context, networkId string, req *common.NormalizedRequest) ([]byte, error) {
	rw := common.NewMemoryResponseWriter()
	crw := common.NewHttpCompositeResponseWriter(rw)

	err := n.Forward(ctx, req, crw)
	if err != nil {
		return nil, err
	}

	respBody, err := io.ReadAll(rw)
	if err != nil {
		return nil, err
	}

	return respBody, nil
}

func (n *PreparedNetwork) EvmGetChainId(ctx context.Context) (string, error) {
	pr := common.NewNormalizedRequest(n.NetworkId, []byte(`{"jsonrpc":"2.0","method":"eth_chainId","params":[]}`))
	respBytes, err := n.Send(ctx, n.NetworkId, pr)
	if err != nil {
		return "", err
	}

	jrr := &common.JsonRpcResponse{}
	err = json.Unmarshal(respBytes, jrr)
	if err != nil {
		return "", err
	}

	if jrr.Error != nil {
		return "", common.WrapJsonRpcError(jrr.Error)
	}

	log.Debug().Msgf("eth_chainId response: %+v", jrr)

	hex, err := common.NormalizeHex(jrr.Result)
	if err != nil {
		return "", err
	}

	dec, err := common.HexToUint64(hex)
	if err != nil {
		return "", err
	}

	return strconv.FormatUint(dec, 10), nil
}

func (n *PreparedNetwork) EvmIsBlockFinalized(blockNumber uint64) (bool, error) {
	if n.evmBlockTracker == nil {
		return false, nil
	}

	finalizedBlock := n.evmBlockTracker.FinalizedBlockNumber
	latestBlock := n.evmBlockTracker.LatestBlockNumber
	if latestBlock == 0 && finalizedBlock == 0 {
		n.Logger.Debug().
			Uint64("finalizedBlock", finalizedBlock).
			Uint64("latestBlock", latestBlock).
			Uint64("blockNumber", blockNumber).
			Msgf("finalized/latest blocks are not available yet when checking block finality")
		return false, nil
	}

	if finalizedBlock > 0 {
		return blockNumber <= finalizedBlock, nil
	}

	if latestBlock == 0 {
		return false, nil
	}

	var fb uint64

	if n.Config.Evm != nil {
		fb = latestBlock - n.Config.Evm.FinalityDepth
	} else {
		fb = latestBlock - 128
	}

	return blockNumber <= fb, nil
}

func (n *PreparedNetwork) resolveNetworkId(ctx context.Context) error {
	if n.NetworkId != "" {
		n.Logger.Trace().Msgf("network id already resolved")
		return nil
	}

	n.Logger.Debug().Msgf("resolving network id")
	if n.Architecture() == "evm" {
		if n.Config.Evm != nil && n.Config.Evm.ChainId > 0 {
			n.NetworkId = strconv.Itoa(n.Config.Evm.ChainId)
		} else {
			nid, err := n.EvmGetChainId(ctx)
			if err != nil {
				return err
			}
			n.NetworkId = nid
		}
	}

	if n.NetworkId == "" {
		return common.NewErrUnknownNetworkID(n.Architecture())
	}

	n.Logger.Debug().Msgf("resolved network id to: %s", n.NetworkId)

	return nil
}

func (n *PreparedNetwork) acquireRateLimitPermit(req *common.NormalizedRequest) error {
	if n.Config.RateLimitBucket == "" {
		return nil
	}

	rlb, errNetLimit := n.rateLimitersRegistry.GetBucket(n.Config.RateLimitBucket)
	if errNetLimit != nil {
		return errNetLimit
	}
	if rlb == nil {
		return nil
	}

	method, errMethod := req.Method()
	if errMethod != nil {
		return errMethod
	}

	rules := rlb.GetRulesByMethod(method)
	n.Logger.Debug().Msgf("found %d network-level rate limiters for network: %s method: %s", len(rules), n.NetworkId, method)

	if len(rules) > 0 {
		for _, rule := range rules {
			permit := (*rule.Limiter).TryAcquirePermit()
			if !permit {
				health.MetricNetworkRequestLocalRateLimited.WithLabelValues(
					n.ProjectId,
					n.NetworkId,
					method,
				).Inc()
				return common.NewErrNetworkRateLimitRuleExceeded(
					n.ProjectId,
					n.NetworkId,
					n.Config.RateLimitBucket,
					rule.Config,
				)
			} else {
				n.Logger.Debug().Object("rateLimitRule", rule.Config).Msgf("network-level rate limit passed")
			}
		}
	}

	return nil
}

func (n *PreparedNetwork) forwardToUpstream(
	thisUpstream *upstream.PreparedUpstream,
	ctx context.Context,
	r interface{},
	w common.ResponseWriter,
) error {
	var category string = ""
	if jrr, ok := r.(*common.JsonRpcRequest); ok {
		category = jrr.Method
	}
	health.MetricUpstreamRequestTotal.WithLabelValues(
		n.ProjectId,
		n.NetworkId,
		thisUpstream.Id,
		category,
	).Inc()
	timer := prometheus.NewTimer(health.MetricUpstreamRequestDuration.WithLabelValues(
		n.ProjectId,
		n.NetworkId,
		thisUpstream.Id,
		category,
	))
	defer timer.ObserveDuration()

	return thisUpstream.Forward(ctx, n.NetworkId, r, w)
}

func weightedRandomSelect(upstreams []*upstream.PreparedUpstream) *upstream.PreparedUpstream {
	totalScore := 0
	for _, upstream := range upstreams {
		totalScore += upstream.Score
	}

	if totalScore == 0 {
		return upstreams[0]
	}

	randomValue := rand.Intn(totalScore)

	for _, upstream := range upstreams {
		if randomValue < upstream.Score {
			return upstream
		}
		randomValue -= upstream.Score
	}

	// This should never be reached
	return upstreams[len(upstreams)-1]
}

func reorderUpstreams(upstreams []*upstream.PreparedUpstream) []*upstream.PreparedUpstream {
	reordered := make([]*upstream.PreparedUpstream, len(upstreams))
	remaining := append([]*upstream.PreparedUpstream{}, upstreams...)

	for i := range reordered {
		selected := weightedRandomSelect(remaining)
		reordered[i] = selected

		// Remove selected item from remaining upstreams
		for j, upstream := range remaining {
			if upstream.Id == selected.Id {
				remaining = append(remaining[:j], remaining[j+1:]...)
				break
			}
		}
	}

	return reordered
}
