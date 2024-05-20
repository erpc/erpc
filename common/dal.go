package common

type DAL interface {
	Get(req *NormalizedRequest) ([]byte, error)
	Set(req *NormalizedRequest, value interface{}) (int, error)
	Delete(req *NormalizedRequest) error
}
