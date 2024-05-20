package common

import "io"

type DAL interface {
	Get(req *NormalizedRequest) (string, error)
	GetWithReader(req *NormalizedRequest) (io.Reader, error)
	Set(req *NormalizedRequest, value string) (int, error)
	SetWithWriter(req *NormalizedRequest) (io.WriteCloser, error)
	Delete(req *NormalizedRequest) error
}
