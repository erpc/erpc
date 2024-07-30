package data

import (
	"context"

	"github.com/erpc/erpc/common"
	"github.com/erpc/erpc/upstream"
)

type CacheDAL interface {
	Set(ctx context.Context, req *upstream.NormalizedRequest, res common.NormalizedResponse) error
	Get(ctx context.Context, req *upstream.NormalizedRequest) (common.NormalizedResponse, error)
	DeleteByGroupKey(ctx context.Context, groupKeys ...string) error
}
