package evm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/bytedance/sonic/ast"
	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/util"
)

type getLogsFilter struct {
	addrAny bool
	addrSet map[string]struct{}

	// topics[i] is:
	// - nil: wildcard
	// - len>0: OR list of accepted topic values (lowercased)
	topics [][]string
}

func newGetLogsFilter(address interface{}, topics interface{}) *getLogsFilter {
	f := &getLogsFilter{
		addrAny: true,
		addrSet: nil,
		topics:  nil,
	}

	// address: string | []any
	switch v := address.(type) {
	case string:
		if v != "" {
			f.addrAny = false
			f.addrSet = map[string]struct{}{strings.ToLower(v): {}}
		}
	case []interface{}:
		set := make(map[string]struct{}, len(v))
		for _, a := range v {
			as, ok := a.(string)
			if !ok || as == "" {
				continue
			}
			set[strings.ToLower(as)] = struct{}{}
		}
		if len(set) > 0 {
			f.addrAny = false
			f.addrSet = set
		}
	}

	// topics: []any; each item: string | []any | nil
	arr, ok := topics.([]interface{})
	if !ok || len(arr) == 0 {
		return f
	}
	f.topics = make([][]string, len(arr))
	for i := range arr {
		if arr[i] == nil {
			f.topics[i] = nil
			continue
		}
		switch tv := arr[i].(type) {
		case string:
			if tv == "" {
				f.topics[i] = []string{}
			} else {
				f.topics[i] = []string{strings.ToLower(tv)}
			}
		case []interface{}:
			out := make([]string, 0, len(tv))
			for _, o := range tv {
				os, ok := o.(string)
				if !ok || os == "" {
					continue
				}
				out = append(out, strings.ToLower(os))
			}
			f.topics[i] = out
		default:
			f.topics[i] = []string{}
		}
	}
	return f
}

func (f *getLogsFilter) matchesLog(log *ast.Node) bool {
	if log == nil || !log.Exists() {
		return false
	}
	if !f.addrAny {
		addrNode := log.Get("address")
		if addrNode == nil || !addrNode.Exists() {
			return false
		}
		addr, err := addrNode.String()
		if err != nil {
			return false
		}
		if _, ok := f.addrSet[strings.ToLower(addr)]; !ok {
			return false
		}
	}

	if len(f.topics) == 0 {
		return true
	}
	topicsNode := log.Get("topics")
	if topicsNode == nil || !topicsNode.Exists() {
		return false
	}

	for i := range f.topics {
		allowed := f.topics[i]
		if allowed == nil {
			continue // wildcard
		}
		if len(allowed) == 0 {
			return false
		}
		topicNode := topicsNode.Index(i)
		if topicNode == nil || !topicNode.Exists() {
			return false
		}
		got, err := topicNode.String()
		if err != nil {
			return false
		}
		got = strings.ToLower(got)
		match := false
		for _, a := range allowed {
			if got == a {
				match = true
				break
			}
		}
		if !match {
			return false
		}
	}

	return true
}

// shouldFallbackEthGetLogsToBlockReceipts returns true for "deterministic too-large" failures
// where splitting by range is impossible (single-block) and we can attempt an alternate
// retrieval method.
func shouldFallbackEthGetLogsToBlockReceipts(err error) bool {
	if err == nil {
		return false
	}
	if common.HasErrorCode(err, common.ErrCodeEndpointRequestTooLarge) {
		return true
	}
	var jre *common.ErrJsonRpcExceptionInternal
	if errors.As(err, &jre) && jre.NormalizedCode() == common.JsonRpcErrorEvmLargeRange {
		return true
	}
	return false
}

type getLogsFromBlockReceiptsWriter struct {
	mu sync.Mutex

	holder *common.NormalizedResponse
	jrr    *common.JsonRpcResponse
	filter *getLogsFilter

	emptyOnce sync.Once
	empty     bool
}

func newGetLogsFromBlockReceiptsWriter(holder *common.NormalizedResponse, jrr *common.JsonRpcResponse, filter *getLogsFilter) *getLogsFromBlockReceiptsWriter {
	return &getLogsFromBlockReceiptsWriter{
		holder: holder,
		jrr:    jrr,
		filter: filter,
		empty:  true,
	}
}

func (w *getLogsFromBlockReceiptsWriter) Release() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.jrr != nil {
		w.jrr.Free()
		w.jrr = nil
	}
	if w.holder != nil {
		w.holder.Release()
		w.holder = nil
	}
	w.filter = nil
}

func (w *getLogsFromBlockReceiptsWriter) IsResultEmptyish() bool {
	w.emptyOnce.Do(func() {
		w.empty = true
		if w.jrr == nil {
			return
		}
		root := ast.NewRawConcurrentRead(util.B2Str(w.jrr.GetResultBytes()))
		root.LoadAll()
		rIter, err := root.Values()
		if err != nil {
			return
		}
		var receipt ast.Node
		for rIter.Next(&receipt) {
			logsNode := receipt.Get("logs")
			if logsNode == nil || !logsNode.Exists() {
				continue
			}
			lIter, err := logsNode.Values()
			if err != nil {
				continue
			}
			var lg ast.Node
			for lIter.Next(&lg) {
				if w.filter.matchesLog(&lg) {
					w.empty = false
					return
				}
			}
		}
	})
	return w.empty
}

func (w *getLogsFromBlockReceiptsWriter) Size(ctx ...context.Context) (int, error) {
	return 0, fmt.Errorf("size unknown")
}

func (w *getLogsFromBlockReceiptsWriter) WriteTo(out io.Writer, trimSides bool) (n int64, err error) {
	w.mu.Lock()
	jrr := w.jrr
	filter := w.filter
	w.mu.Unlock()

	if jrr == nil || filter == nil {
		return 0, nil
	}

	// Write opening bracket
	if !trimSides {
		nn, err := out.Write([]byte{'['})
		if err != nil {
			return int64(nn), err
		}
		n += int64(nn)
	}

	first := true
	root := ast.NewRawConcurrentRead(util.B2Str(jrr.GetResultBytes()))
	if err := root.LoadAll(); err != nil {
		return n, err
	}
	rIter, err := root.Values()
	if err != nil {
		return n, err
	}

	var receipt ast.Node
	for rIter.Next(&receipt) {
		logsNode := receipt.Get("logs")
		if logsNode == nil || !logsNode.Exists() {
			continue
		}
		lIter, err := logsNode.Values()
		if err != nil {
			continue
		}
		var lg ast.Node
		for lIter.Next(&lg) {
			if !filter.matchesLog(&lg) {
				continue
			}
			raw, err := lg.Raw()
			if err != nil {
				return n, err
			}
			if !first {
				nn, err := out.Write([]byte{','})
				if err != nil {
					return n + int64(nn), err
				}
				n += int64(nn)
			}
			first = false

			nn, err := out.Write([]byte(raw))
			if err != nil {
				return n + int64(nn), err
			}
			n += int64(nn)
		}
	}

	w.emptyOnce.Do(func() { w.empty = first })

	// Write closing bracket
	if !trimSides {
		nn, err := out.Write([]byte{']'})
		return n + int64(nn), err
	}
	return n, nil
}

func fallbackEthGetLogsSingleBlockViaBlockReceipts(ctx context.Context, n common.Network, parent *common.NormalizedRequest, blockNumber int64, address interface{}, topics interface{}, skipCacheRead bool, responseID interface{}) (*common.JsonRpcResponse, error) {
	bh, err := common.NormalizeHex(blockNumber)
	if err != nil {
		return nil, err
	}
	jrq := common.NewJsonRpcRequest("eth_getBlockReceipts", []interface{}{bh})
	if err := jrq.SetID(util.RandomID()); err != nil {
		return nil, err
	}
	nrq := common.NewNormalizedRequestFromJsonRpcRequest(jrq)
	dr := parent.Directives().Clone()
	dr.SkipCacheRead = skipCacheRead
	nrq.SetDirectives(dr)
	nrq.SetNetwork(n)
	nrq.SetParentRequestId(parent.ID())
	nrq.CopyHttpContextFrom(parent)

	rs, re := n.Forward(ctx, nrq)
	if re != nil {
		return nil, re
	}
	jrr, err := rs.JsonRpcResponse(ctx)
	if err != nil {
		rs.Release()
		return nil, err
	}
	if jrr == nil {
		rs.Release()
		return nil, fmt.Errorf("unexpected empty json-rpc response %v", rs)
	}
	if jrr.Error != nil {
		rs.Release()
		return nil, jrr.Error
	}

	idBytes, err := common.SonicCfg.Marshal(responseID)
	if err != nil {
		rs.Release()
		return nil, err
	}
	out, err := common.NewJsonRpcResponseFromBytes(idBytes, nil, nil)
	if err != nil {
		rs.Release()
		return nil, err
	}
	out.SetResultWriter(newGetLogsFromBlockReceiptsWriter(rs, jrr, newGetLogsFilter(address, topics)))
	return out, nil
}
