package resiliency

import (
	"errors"
	"fmt"
	"time"

	"github.com/failsafe-go/failsafe-go"
	"github.com/failsafe-go/failsafe-go/hedgepolicy"
	"github.com/failsafe-go/failsafe-go/retrypolicy"
	"github.com/failsafe-go/failsafe-go/timeout"
	"github.com/flair-sdk/erpc/common"
	"github.com/flair-sdk/erpc/config"
)

func CreateFailSafePolicies(component string, fsCfg *config.FailsafeConfig) ([]failsafe.Policy[any], error) {
	var policies = []failsafe.Policy[any]{}

	if fsCfg == nil {
		return policies, nil
	}

	if fsCfg.Hedge != nil {
		p, err := createHegePolicy(component, fsCfg.Hedge)
		if err != nil {
			return nil, err
		}
		policies = append(policies, p)
	}

	if fsCfg.Retry != nil {
		p, err := createRetryPolicy(component, fsCfg.Retry)
		if err != nil {
			return nil, err
		}
		policies = append(policies, p)
	}

	if fsCfg.Timeout != nil {
		p, err := createTimeoutPolicy(component, fsCfg.Timeout)
		if err != nil {
			return nil, err
		}
		policies = append(policies, p)
	}

	return policies, nil
}

func createHegePolicy(component string, cfg *config.HedgePolicyConfig) (failsafe.Policy[any], error) {
	delay, err := time.ParseDuration(cfg.Delay)
	if err != nil {
		return nil, common.NewErrFailsafeConfiguration(fmt.Errorf("failed to parse hedge.delay: %v", err), map[string]interface{}{
			"component": component,
			"policy":    cfg,
		})
	}
	builder := hedgepolicy.BuilderWithDelay[interface{}](delay)

	if cfg.MaxCount > 0 {
		builder = builder.WithMaxHedges(cfg.MaxCount)
	}

	return builder.Build(), nil
}

func createRetryPolicy(component string, cfg *config.RetryPolicyConfig) (failsafe.Policy[any], error) {
	builder := retrypolicy.Builder[any]()

	if cfg.MaxAttempts > 0 {
		builder = builder.WithMaxAttempts(cfg.MaxAttempts)
	}
	if cfg.Delay != "" {
		delayDuration, err := time.ParseDuration(cfg.Delay)
		if err != nil {
			return nil, common.NewErrFailsafeConfiguration(fmt.Errorf("failed to parse retry.delay: %v", err), map[string]interface{}{
				"component": component,
				"policy":    cfg,
			})
		}

		if cfg.BackoffMaxDelay != "" {
			backoffMaxDuration, err := time.ParseDuration(cfg.BackoffMaxDelay)
			if err != nil {
				return nil, common.NewErrFailsafeConfiguration(fmt.Errorf("failed to parse retry.backoffMaxDelay: %v", err), map[string]interface{}{
					"component": component,
					"policy":    cfg,
				})
			}

			if cfg.BackoffFactor > 0 {
				builder = builder.WithBackoffFactor(delayDuration, backoffMaxDuration, cfg.BackoffFactor)
			} else {
				builder = builder.WithBackoff(delayDuration, backoffMaxDuration)
			}
		} else {
			builder = builder.WithDelay(delayDuration)
		}
	}
	if cfg.Jitter != "" {
		jitterDuration, err := time.ParseDuration(cfg.Jitter)
		if err != nil {
			return nil, common.NewErrFailsafeConfiguration(fmt.Errorf("failed to parse retry.jitter: %v", err), map[string]interface{}{
				"component": component,
				"policy":    cfg,
			})
		}

		builder = builder.WithJitter(jitterDuration)
	}

	return builder.Build(), nil
}

func createTimeoutPolicy(component string, cfg *config.TimeoutPolicyConfig) (failsafe.Policy[any], error) {
	if cfg.Duration == "" {
		return nil, common.NewErrFailsafeConfiguration(errors.New("missing timeout"), map[string]interface{}{
			"component": component,
			"policy":    cfg,
		})
	}

	timeoutDuration, err := time.ParseDuration(cfg.Duration)
	builder := timeout.Builder[any](timeoutDuration)

	if err != nil {
		return nil, common.NewErrFailsafeConfiguration(fmt.Errorf("failed to parse timeout: %v", err), map[string]interface{}{
			"component": component,
			"policy":    cfg,
		})
	}

	return builder.Build(), nil
}

func TranslateFailsafeError(execErr error) error {
	var retryExceededErr *retrypolicy.ExceededError
	if errors.As(execErr, &retryExceededErr) {
		return common.NewErrFailsafeRetryExceeded(
			retryExceededErr.LastError(),
			retryExceededErr.LastResult(),
		)
	}

	if errors.Is(execErr, timeout.ErrExceeded) {
		return common.NewErrFailsafeTimeoutExceeded(execErr)
	}

	return &common.BaseError{
		Code:    "ErrFailsafeUnexpected",
		Message: "unexpected failsafe error type encountered",
		Cause:   execErr,
	}
}
