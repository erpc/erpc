package data

import (
	"context"

	"github.com/erpc/erpc/common"
)

type EvmCacheAware interface {
	SetEvmCache(ctx context.Context, req *common.NormalizedRequest, resp *common.NormalizedResponse) error
	GetEvmCache(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error)
}
