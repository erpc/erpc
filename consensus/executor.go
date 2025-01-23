package consensus

import (
	"github.com/failsafe-go/failsafe-go"
	"github.com/failsafe-go/failsafe-go/common"
	"github.com/failsafe-go/failsafe-go/policy"
)

// executor is a policy.Executor that handles failures according to a ConsensusPolicy.
type executor[R any] struct {
	*policy.BaseExecutor[R]
	*consensusPolicy[R]
}

var _ policy.Executor[any] = &executor[any]{}

func (e *executor[R]) Apply(innerFn func(failsafe.Execution[R]) *common.PolicyResult[R]) func(failsafe.Execution[R]) *common.PolicyResult[R] {
	return func(exec failsafe.Execution[R]) *common.PolicyResult[R] {
		// TODO implement consensus logic
		return innerFn(exec)

		// FIXME some code from hedge to get inspiration from:
		//
		// type execResult struct {
		// 	result *common.PolicyResult[R]
		// 	index  int
		// }
		// parentExecution := exec.(policy.ExecutionInternal[R])
		// executions := make([]policy.ExecutionInternal[R], e.requiredParticipants)

		// // Guard against a race between execution results
		// resultCount := atomic.Int32{}
		// resultSent := atomic.Bool{}
		// resultChan := make(chan *execResult, 1) // Only one result is sent

		// for execIdx := 0; ; execIdx++ {
		// 	// Prepare execution
		// 	executions[execIdx] = parentExecution.CopyForCancellable().(policy.ExecutionInternal[R])

		// 	// Perform execution
		// 	go func(consensusExec policy.ExecutionInternal[R], execIdx int) {
		// 		result := innerFn(consensusExec)
		// 		isFinalResult := int(resultCount.Add(1)) == e.requiredParticipants
		// 		isCancellable := e.IsAbortable(result.Result, result.Error)
		// 		if (isFinalResult || isCancellable) && resultSent.CompareAndSwap(false, true) {
		// 			resultChan <- &execResult{result, execIdx}
		// 		}
		// 	}(executions[execIdx], execIdx)

		// 	var result *execResult
		// 	if execIdx < e.requiredParticipants {
		// 		// timer := time.NewTimer(e.delayFunc(exec))
		// 		// select {
		// 		// case <-timer.C:
		// 		// case result = <-resultChan:
		// 		// 	timer.Stop()
		// 		// }
		// 		result = <-resultChan
		// 	} else {
		// 		result = <-resultChan
		// 	}

		// 	// Return if parent execution is canceled
		// 	if canceled, cancelResult := parentExecution.IsCanceledWithResult(); canceled {
		// 		return cancelResult
		// 	}

		// 	// Return result and cancel any outstanding attempts
		// 	if result != nil {
		// 		for i, execution := range executions {
		// 			if i != result.index && execution != nil {
		// 				execution.Cancel(nil)
		// 			}
		// 		}
		// 		return result.result
		// 	}
		// }
	}
}
