package data

import (
	"context"
	"io"

	"github.com/flair-sdk/erpc/upstream"
)

type DAL interface {
	GetWithReader(ctx context.Context, req *upstream.NormalizedRequest) (io.Reader, error)
	SetWithWriter(ctx context.Context, req *upstream.NormalizedRequest) (io.WriteCloser, error)
	Delete(ctx context.Context, req *upstream.NormalizedRequest) error
}
