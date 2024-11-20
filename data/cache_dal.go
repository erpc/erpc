package data

import (
	"context"

	"github.com/erpc/erpc/common"
)

type CacheDAL interface {
	Set(ctx context.Context, req *common.NormalizedRequest, res *common.NormalizedResponse) error
	Get(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error)
}
