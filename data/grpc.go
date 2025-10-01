package data

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/blockchain-data-standards/manifesto/evm"
	"github.com/bytedance/sonic"
	"github.com/erpc/erpc/clients"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
)

const GrpcDriverName = "grpc"

// GrpcConnector implements a read-through connector backed by BDS gRPC servers.
// It is read-only: Set/Delete/List/Lock/Watch/Publish are no-ops or unsupported.
// It bootstraps from an endpoint that returns a list of gRPC server URLs.
// Each server is probed for chainId to map it to a network.
// A background poller tracks the earliest available block per network for fast misses.
type GrpcConnector struct {
	id     string
	logger *zerolog.Logger

	appCtx context.Context
	mu     sync.RWMutex
	// networkId -> single client
	clientByNetwork map[string]clients.GrpcBdsClient
	// earliest block per network (0 if unknown)
	earliestByNetwork map[string]uint64
	// headers to apply to all clients
	headers     map[string]string
	initializer *util.Initializer
}

var _ Connector = (*GrpcConnector)(nil)

// supportedMethods is a fast allowlist for methods served by gRPC BDS.
var supportedMethods = map[string]struct{}{
	"eth_getBlockByNumber":      {},
	"eth_getBlockByHash":        {},
	"eth_getLogs":               {},
	"eth_getTransactionByHash":  {},
	"eth_getTransactionReceipt": {},
	"eth_getBlockReceipts":      {},
	"eth_chainId":               {},
}

func NewGrpcConnector(
	ctx context.Context,
	logger *zerolog.Logger,
	id string,
	cfg *common.GrpcConnectorConfig,
) (*GrpcConnector, error) {
	if cfg == nil {
		return nil, fmt.Errorf("grpc connector requires config")
	}

	lg := logger.With().Str("connector", id).Logger()

	var servers []string
	// Prefer explicit servers if provided
	if len(cfg.Servers) > 0 {
		servers = append(servers, cfg.Servers...)
	}

	if cfg.Bootstrap != "" {
		var err error
		servers, err = fetchGrpcServers(ctx, &lg, cfg.Bootstrap)
		if err != nil {
			return nil, fmt.Errorf("bootstrap failed: %w", err)
		}
	}
	if len(servers) == 0 {
		lg.Warn().Msg("no gRPC servers provided or discovered")
	}

	gc := &GrpcConnector{
		id:                id,
		logger:            &lg,
		appCtx:            ctx,
		clientByNetwork:   make(map[string]clients.GrpcBdsClient),
		earliestByNetwork: make(map[string]uint64),
		headers:           map[string]string{},
		initializer:       util.NewInitializer(ctx, &lg, nil),
	}
	if cfg.Headers != nil {
		// Make a copy of the headers map to avoid sharing the same reference
		gc.headers = make(map[string]string, len(cfg.Headers))
		for k, v := range cfg.Headers {
			gc.headers[k] = v
		}
	}

	// Create one connect task per server and let the initializer handle retries
	var tasks []*util.BootstrapTask
	for _, s := range servers {
		serverURL := s
		tasks = append(tasks, util.NewBootstrapTask(
			fmt.Sprintf("grpc-connect/%s", serverURL),
			func(tctx context.Context) error {
				parsed, perr := url.Parse(serverURL)
				if perr != nil {
					gc.logger.Error().Err(perr).Str("server", serverURL).Msg("invalid gRPC server URL")
					return perr
				}
				cli, cerr := clients.NewGrpcBdsClient(gc.appCtx, &lg, "<cache>", nil, parsed)
				if cerr != nil {
					gc.logger.Warn().Err(cerr).Str("server", serverURL).Msg("failed to create gRPC client")
					return cerr
				}
				// Apply headers
				if len(gc.headers) > 0 {
					cli.SetHeaders(gc.headers)
				}
				// Probe chainId with a short timeout derived from task context
				probeCtx, cancel := context.WithTimeout(tctx, 5*time.Second)
				defer cancel()
				nrq := common.NewNormalizedRequestFromJsonRpcRequest(common.NewJsonRpcRequest("eth_chainId", nil))
				resp, rerr := cli.SendRequest(probeCtx, nrq)
				if rerr != nil {
					gc.logger.Warn().Err(rerr).Str("server", serverURL).Msg("chainId probe failed")
					return rerr
				}
				jrr, jerr := resp.JsonRpcResponse(probeCtx)
				if jerr != nil || jrr == nil {
					gc.logger.Warn().Err(jerr).Str("server", serverURL).Msg("chainId response parse failed")
					if jerr != nil {
						return jerr
					}
					return fmt.Errorf("empty chainId response")
				}
				var chainHex string
				if err := common.SonicCfg.Unmarshal(jrr.GetResultBytes(), &chainHex); err != nil || chainHex == "" {
					gc.logger.Warn().Err(err).Str("server", serverURL).Msg("invalid chainId result")
					if err != nil {
						return err
					}
					return fmt.Errorf("invalid chainId")
				}
				// normalize to network id
				uval, _ := evm.HexToUint64(chainHex)
				val := int64(uval) // #nosec G115
				networkId := util.EvmNetworkId(val)
				gc.mu.Lock()
				defer gc.mu.Unlock()
				if existing := gc.clientByNetwork[networkId]; existing != nil {
					gc.logger.Error().Str("server", serverURL).Str("networkId", networkId).Msg("duplicate gRPC server for network detected; ignoring")
					return nil
				}
				gc.clientByNetwork[networkId] = cli
				gc.logger.Info().Str("server", serverURL).Str("networkId", networkId).Msg("gRPC client initialized for network")
				return nil
			},
		))
	}
	if len(tasks) > 0 {
		if err := gc.initializer.ExecuteTasks(ctx, tasks...); err != nil {
			lg.Error().Err(err).Interface("status", gc.initializer.Status()).Msg("failed to initialize gRPC clients on first attempt (will keep retrying in the background)")
		}
	}

	// Start earliest poller per network
	go gc.startEarliestPoller(ctx)

	return gc, nil
}

func (g *GrpcConnector) Id() string { return g.id }

func (g *GrpcConnector) Get(ctx context.Context, index, partitionKey, rangeKey string, metadata interface{}) ([]byte, error) {
	// Expect metadata to be *common.NormalizedRequest
	req, ok := metadata.(*common.NormalizedRequest)
	if !ok || req == nil {
		return nil, common.NewErrRecordNotFound(partitionKey, rangeKey, GrpcDriverName)
	}
	method, _ := req.Method()
	if method == "" {
		return nil, common.NewErrRecordNotFound(partitionKey, rangeKey, GrpcDriverName)
	}
	if _, supported := supportedMethods[method]; !supported {
		return nil, nil // fast skip
	}
	if err := g.checkReady(); err != nil {
		return nil, err
	}
	networkId := req.NetworkId()
	g.mu.RLock()
	cli := g.clientByNetwork[networkId]
	earliest := g.earliestByNetwork[networkId]
	g.mu.RUnlock()
	if cli == nil {
		return nil, common.NewErrRecordNotFound(partitionKey, rangeKey, GrpcDriverName)
	}

	// Fast reject if request block number is before earliest known
	if bni := req.EvmBlockNumber(); bni != nil && earliest > 0 {
		if bn, ok := bni.(uint64); ok && bn < earliest {
			return nil, common.NewErrRecordNotFound(partitionKey, rangeKey, GrpcDriverName)
		}
	}

	resp, err := cli.SendRequest(ctx, req)
	if err != nil || resp == nil {
		return nil, err
	}
	jrr, err := resp.JsonRpcResponse(ctx)
	if err != nil || jrr == nil {
		return nil, common.NewErrRecordNotFound(partitionKey, rangeKey, GrpcDriverName)
	}
	return jrr.GetResultBytes(), nil
}

func (g *GrpcConnector) startEarliestPoller(ctx context.Context) {
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			g.pollEarliestOnce(ctx)
		}
	}
}

func (g *GrpcConnector) pollEarliestOnce(ctx context.Context) {
	g.mu.RLock()
	// snapshot to avoid holding lock during RPCs
	snapshot := make(map[string]clients.GrpcBdsClient)
	for nid, cli := range g.clientByNetwork {
		if cli != nil {
			snapshot[nid] = cli
		}
	}
	g.mu.RUnlock()

	for nid, cli := range snapshot {
		jr := common.NewJsonRpcRequest("eth_getBlockByNumber", []interface{}{"earliest", false})
		nrq := common.NewNormalizedRequestFromJsonRpcRequest(jr)
		resp, err := cli.SendRequest(ctx, nrq)
		if err != nil || resp == nil {
			continue
		}
		jrr, err := resp.JsonRpcResponse(ctx)
		if err != nil || jrr == nil {
			continue
		}
		// Parse number field
		num := parseBlockNumberHex(jrr.GetResultBytes())
		if num == "" {
			continue
		}
		u, err := evm.HexToUint64(num)
		if err != nil {
			continue
		}
		g.mu.Lock()
		g.earliestByNetwork[nid] = u
		g.mu.Unlock()
	}
}

func parseBlockNumberHex(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	node, err := sonic.Get(b, "number")
	if err == nil {
		v, _ := node.String()
		return v
	}
	return ""
}

func fetchGrpcServers(ctx context.Context, logger *zerolog.Logger, endpoint string) ([]string, error) {
	// Simple HTTP GET that returns a JSON array of URLs or newline separated text
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var arr []string
	if err := sonic.Unmarshal(body, &arr); err == nil && len(arr) > 0 {
		return arr, nil
	}
	// Fallback to newline separated
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l != "" {
			out = append(out, l)
		}
	}
	logger.Debug().Strs("servers", out).Msg("discovered gRPC cache servers")
	return out, nil
}

func (g *GrpcConnector) Set(ctx context.Context, partitionKey, rangeKey string, value []byte, ttl *time.Duration) error {
	// no-op for read-only connector
	return fmt.Errorf("grpc connector is read-only")
}

func (g *GrpcConnector) Delete(ctx context.Context, partitionKey, rangeKey string) error {
	return fmt.Errorf("grpc connector is read-only")
}

func (g *GrpcConnector) List(ctx context.Context, index string, limit int, paginationToken string) ([]KeyValuePair, string, error) {
	return nil, "", fmt.Errorf("grpc connector is does not support List")
}

func (g *GrpcConnector) Lock(ctx context.Context, key string, ttl time.Duration) (DistributedLock, error) {
	return nil, fmt.Errorf("grpc connector is read-only")
}

func (g *GrpcConnector) WatchCounterInt64(ctx context.Context, key string) (<-chan int64, func(), error) {
	ch := make(chan int64)
	return ch, func() {}, fmt.Errorf("grpc connector does not support WatchCounterInt64")
}

func (g *GrpcConnector) PublishCounterInt64(ctx context.Context, key string, value int64) error {
	return fmt.Errorf("grpc connector does not support PublishCounterInt64")
}

func (g *GrpcConnector) checkReady() error {
	if g.initializer == nil {
		return fmt.Errorf("initializer not set")
	}
	g.mu.RLock()
	empty := len(g.clientByNetwork) == 0
	g.mu.RUnlock()
	if empty {
		return fmt.Errorf("no gRPC clients initialized")
	}
	return nil
}
