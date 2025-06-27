package common

import (
	"context"
)

type CacheDAL interface {
	Set(ctx context.Context, req *NormalizedRequest, res *NormalizedResponse) error
	Get(ctx context.Context, req *NormalizedRequest) (*NormalizedResponse, error)
	IsObjectNull() bool
}
