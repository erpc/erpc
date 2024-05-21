package common

import (
	"context"
	"io"
)

type DAL interface {
	Get(ctx context.Context, req *NormalizedRequest) (string, error)
	GetWithReader(ctx context.Context, req *NormalizedRequest) (io.Reader, error)
	Set(ctx context.Context, req *NormalizedRequest, value string) (int, error)
	SetWithWriter(ctx context.Context, req *NormalizedRequest) (io.WriteCloser, error)
	Delete(ctx context.Context, req *NormalizedRequest) error
}
