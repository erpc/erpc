package resiliency

import (
	"errors"
	"fmt"
	"time"

	"github.com/failsafe-go/failsafe-go"
	"github.com/failsafe-go/failsafe-go/retrypolicy"
	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/config"
)

func CreateFailSafePolicies(component string, fsCfg *config.FailsafeConfig) ([]failsafe.Policy[any], error) {
	var policies = []failsafe.Policy[any]{}

	if fsCfg == nil {
		return policies, nil
	}

	if fsCfg.Retry != nil {
		builder := retrypolicy.Builder[any]()
		if fsCfg.Retry.MaxAttempts > 0 {
			builder = builder.WithMaxAttempts(fsCfg.Retry.MaxAttempts)
		}
		if fsCfg.Retry.Delay != "" {
			delayDuration, err := time.ParseDuration(fsCfg.Retry.Delay)
			if err != nil {
				return nil, common.NewErrFailsafeConfiguration(fmt.Errorf("failed to parse retry.delay: %v", err), map[string]interface{}{
					"component": component,
					"policy":    fsCfg.Retry,
				})
			}

			if fsCfg.Retry.BackoffMaxDelay != "" {
				backoffMaxDuration, err := time.ParseDuration(fsCfg.Retry.BackoffMaxDelay)
				if err != nil {
					return nil, common.NewErrFailsafeConfiguration(fmt.Errorf("failed to parse retry.backoffMaxDelay: %v", err), map[string]interface{}{
						"component": component,
						"policy":    fsCfg.Retry,
					})
				}

				if fsCfg.Retry.BackoffFactor > 0 {
					builder = builder.WithBackoffFactor(delayDuration, backoffMaxDuration, fsCfg.Retry.BackoffFactor)
				} else {
					builder = builder.WithBackoff(delayDuration, backoffMaxDuration)
				}
			} else {
				builder = builder.WithDelay(delayDuration)
			}
		}
		if fsCfg.Retry.Jitter != "" {
			jitterDuration, err := time.ParseDuration(fsCfg.Retry.Jitter)
			if err != nil {
				return nil, common.NewErrFailsafeConfiguration(fmt.Errorf("failed to parse retry.jitter: %v", err), map[string]interface{}{
					"component": component,
					"policy":    fsCfg.Retry,
				})
			}

			builder = builder.WithJitter(jitterDuration)
		}
		policies = append(policies, builder.Build())
	}

	return policies, nil
}

func TranslateFailsafeError(execErr error) *common.BaseError {
	var retryExceededErr *retrypolicy.ExceededError
	if errors.As(execErr, &retryExceededErr) {
		return &common.BaseError{
			Code:    "ErrFailsafeRetryExceeded",
			Message: "failsafe retry policy exceeded for upstream",
			Cause:   retryExceededErr.LastError(),
			Details: map[string]interface{}{
				"lastResult": retryExceededErr.LastResult(),
			},
		}
	}

	return &common.BaseError{
		Code:    "ErrFailsafeUnexpected",
		Message: "unexpected failsafe error type encountered",
		Cause:   execErr,
	}
}
