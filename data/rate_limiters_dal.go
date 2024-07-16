package data

import "context"

type RateLimitersDAL interface {
	Set(ctx context.Context, budget, rule string, value int32) error
	Increment(ctx context.Context, budget, rule string, amount int32) (int32, error)
	Get(ctx context.Context, budget, rule string) (int32, error)
}
