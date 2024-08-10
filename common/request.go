package common

type RequestDirectives struct {
	RetryEmpty bool
}

type NormalizedRequest interface {
	Network() Network
	Method() (string, error)
	Body() []byte
	Directives() *RequestDirectives
	EvmBlockNumber() (uint64, error)

	LastUpstream() Upstream
	LastValidResponse() NormalizedResponse
}

type NormalizedResponse interface {
	Request() NormalizedRequest
	Body() []byte
	JsonRpcResponse() (*JsonRpcResponse, error)
	IsResultEmptyish() bool
	IsObjectNull() bool
}
