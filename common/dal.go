package common

import (
	"context"
	"io"
)

type DAL interface {
	GetWithReader(ctx context.Context, req *NormalizedRequest) (io.Reader, error)
	SetWithWriter(ctx context.Context, req *NormalizedRequest) (io.WriteCloser, error)
	Delete(ctx context.Context, req *NormalizedRequest) error
}
