package common

import (
	"context"
	"time"

	"github.com/rs/zerolog"
)

type User struct {
	Id              string
	RateLimitBudget string

	// X402SettleFunc is set by the x402 auth strategy when using the "upTo"
	// payment scheme. Settlement is deferred until after a successful upstream
	// response so the payer is only charged when they receive value.
	// For "exact" scheme this is nil — settlement happens during auth.
	X402SettleFunc func(ctx context.Context) error
}

// SettleX402 calls the deferred x402 settlement if one is pending. Returns nil
// if no settlement is needed (non-x402 user, exact scheme, or already settled).
//
// On success the func is cleared to prevent double-settlement. On failure the
// func is preserved so the caller can retry.
func (u *User) SettleX402(ctx context.Context) error {
	if u == nil || u.X402SettleFunc == nil {
		return nil
	}
	err := u.X402SettleFunc(ctx)
	if err == nil {
		u.X402SettleFunc = nil
	}
	return err
}

// SettleX402WithRetry attempts deferred settlement and, on failure, retries in
// a background goroutine with exponential backoff (1s, 2s, 4s … up to
// maxRetries). The caller gets their response immediately regardless.
func (u *User) SettleX402WithRetry(ctx context.Context, logger *zerolog.Logger, maxRetries int) {
	if u == nil || u.X402SettleFunc == nil {
		return
	}

	// First attempt uses the request context.
	if err := u.SettleX402(ctx); err == nil {
		return
	}

	// Settlement failed — retry in the background with a detached context.
	// The payment signature is cryptographically valid, so failures here are
	// transient facilitator issues (429, 500, network blip) worth retrying.
	settleFn := u.X402SettleFunc
	u.X402SettleFunc = nil // prevent concurrent callers
	userId := u.Id

	go func() {
		backoff := 1 * time.Second
		for attempt := 1; attempt <= maxRetries; attempt++ {
			time.Sleep(backoff)
			attemptCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			err := settleFn(attemptCtx)
			cancel()
			if err == nil {
				logger.Info().
					Str("userId", userId).
					Int("attempt", attempt).
					Msg("x402 deferred settlement succeeded on retry")
				return
			}
			logger.Warn().Err(err).
				Str("userId", userId).
				Int("attempt", attempt).
				Int("maxRetries", maxRetries).
				Msg("x402 deferred settlement retry failed")
			backoff *= 2
		}
		logger.Error().
			Str("userId", userId).
			Int("maxRetries", maxRetries).
			Msg("x402 deferred settlement exhausted all retries — payment not collected")
	}()
}
