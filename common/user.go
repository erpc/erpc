package common

import "context"

type User struct {
	Id              string
	RateLimitBudget string

	// X402SettleFunc, when set, is called after a successful upstream response
	// to collect payment for the "upto" scheme. Nil for "exact" or non-x402.
	X402SettleFunc func(ctx context.Context) error
}
