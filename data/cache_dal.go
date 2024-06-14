package data

import (
	"context"

	"github.com/flair-sdk/erpc/common"
)

type CacheDAL interface {
	Set(ctx context.Context, req *common.NormalizedRequest, res *common.NormalizedResponse) error
	Get(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error)
	DeleteByGroupKey(ctx context.Context, groupKeys ...string) error
}
