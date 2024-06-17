package data

import (
	"context"

	"github.com/flair-sdk/erpc/upstream"
)

type CacheDAL interface {
	Set(ctx context.Context, req *upstream.NormalizedRequest, res *upstream.NormalizedResponse) error
	Get(ctx context.Context, req *upstream.NormalizedRequest) (*upstream.NormalizedResponse, error)
	DeleteByGroupKey(ctx context.Context, groupKeys ...string) error
}
