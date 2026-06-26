package integrity

import (
	"context"

	"github.com/erpc/erpc/common"
)

// schemaConformance — the result decodes to the structural shape expected for
// the method (an array of receipts, a block object, …). This restores the
// decode-or-reject guard the inline validators had and is the foundation other
// checks rely on (a garbled result is rejected before deeper checks run rather
// than silently treated as empty). Scoped to the aggregate methods, where the
// original guard lived.
func init() {
	register(&Check{
		ID: "schemaConformance", Family: FamilyShape, Class: Deterministic,
		Methods: []string{MethodGetBlockReceipts, MethodGetBlockByNumber, MethodGetBlockByHash},
		Run: func(ctx context.Context, d *Decoded, cfg CheckConfig) *Violation {
			var target any
			switch d.method {
			case MethodGetBlockReceipts:
				target = &[]Receipt{}
			case MethodGetBlockByNumber, MethodGetBlockByHash:
				target = &Header{}
			default:
				return nil
			}
			if err := common.SonicCfg.Unmarshal(d.raw, target); err != nil {
				return failf("malformed result for %s: %v", d.method, err)
			}
			return nil
		},
	})
}
