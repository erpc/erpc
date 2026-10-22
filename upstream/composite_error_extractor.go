package upstream

import (
	"net/http"
	"sort"

	"github.com/erpc/erpc/common"
)

// CompositeJsonRpcErrorExtractor tries each registered architecture's extractor in order
// and returns the first non-nil result. This allows a single ClientRegistry to serve
// multiple architectures without knowing which one owns a given error.
type CompositeJsonRpcErrorExtractor struct {
	extractors []common.JsonRpcErrorExtractor
}

func NewCompositeJsonRpcErrorExtractor() *CompositeJsonRpcErrorExtractor {
	// Iterate architectures in sorted order so extractor composition is
	// deterministic across process restarts. Today each extractor type-guards
	// on Upstream.Config().Type so ordering is semantically irrelevant, but
	// sorting removes the latent failure mode where a future extractor forgets
	// the guard and silently depends on Go's randomized map iteration.
	names := make([]string, 0, len(common.ArchitectureRegistry))
	for name := range common.ArchitectureRegistry {
		names = append(names, string(name))
	}
	sort.Strings(names)
	c := &CompositeJsonRpcErrorExtractor{}
	for _, name := range names {
		c.extractors = append(c.extractors, common.ArchitectureRegistry[common.NetworkArchitecture(name)].NewJsonRpcErrorExtractor())
	}
	return c
}

func (c *CompositeJsonRpcErrorExtractor) Extract(resp *http.Response, nr *common.NormalizedResponse, jr *common.JsonRpcResponse, upstream common.Upstream) error {
	for _, e := range c.extractors {
		if extracted := e.Extract(resp, nr, jr, upstream); extracted != nil {
			return extracted
		}
	}
	return nil
}
