package data

import (
	"context"
	"io"

	"github.com/flair-sdk/erpc/common"
)

type CacheDAL interface {
	SetWithWriter(ctx context.Context, req *common.NormalizedRequest) (io.WriteCloser, error)
	GetWithReader(ctx context.Context, req *common.NormalizedRequest) (io.Reader, error)
	DeleteByGroupKey(ctx context.Context, groupKeys ...string) error
}
