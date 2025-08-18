package common

// Release cleans up all resources held by the NormalizedRequest
// This MUST be called when the request is no longer needed to prevent memory leaks
func (r *NormalizedRequest) Release() {
	if r == nil {
		return
	}

	// Release the lastValidResponse if it exists
	if lvr := r.lastValidResponse.Swap(nil); lvr != nil {
		// lvr.Release()
	}

	// // Clear all map references to allow GC
	// r.ErrorsByUpstream.Range(func(key, value interface{}) bool {
	// 	r.ErrorsByUpstream.Delete(key)
	// 	return true
	// })

	// r.EmptyResponses.Range(func(key, value interface{}) bool {
	// 	r.EmptyResponses.Delete(key)
	// 	return true
	// })

	// if r.ConsumedUpstreams != nil {
	// 	r.ConsumedUpstreams.Range(func(key, value interface{}) bool {
	// 		r.ConsumedUpstreams.Delete(key)
	// 		return true
	// 	})
	// }

	// Clear other references
	// r.lastUpstream.Store(nil)
	// r.evmBlockRef.Store(nil)
	// r.evmBlockNumber.Store(nil)
	// r.network = nil
	// r.cacheDal = nil
	// r.upstreamList = nil
	// r.user.Store(nil)

	// Clear the JSON-RPC request reference
	r.jsonRpcRequest.Store(nil)

	// Clear body to free memory
	// r.body = nil
}
