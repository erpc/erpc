package common

type RequestDirectives struct {
	RetryEmpty bool
}

type NormalizedRequest interface {
	Network() Network
	Upstream() Upstream
	Method() (string, error)
	Directives() *RequestDirectives
	EvmBlockNumber() (uint64, error)
	Clone() NormalizedRequest
}

type NormalizedResponse interface {
	Request() NormalizedRequest
	Body() []byte
	JsonRpcResponse() (*JsonRpcResponse, error)
	IsResultEmptyish() bool
	IsObjectNull() bool
}
