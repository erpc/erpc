package common

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/bytedance/sonic/ast"
	"github.com/erpc/erpc/util"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

var defaultCanonicalHashPlaceholder = interface{}(0) // placeholder for canonical hash

type JsonRpcResponse struct {
	// ID is mainly set based on incoming request.
	// During parsing of response from upstream we'll still parse it
	// so that batch requests are correctly identified.
	// The "id" field is already parsed-value used within erpc,
	// and idBytes is a lazy-loaded byte representation of the ID used when writing responses.
	id      interface{}
	idBytes []byte
	idMu    sync.RWMutex

	// Error is the parsed error from the response bytes, and potentially normalized based on vendor-specific drivers.
	// errBytes is a lazy-loaded byte representation of the Error used when writing responses.
	// When upstream response is received we're going to set the errBytes to incoming bytes and parse the error.
	// Internal components (any error not initiated by upstream) will set the Error field directly and then we'll
	// lazy-load the errBytes for writing responses by marshalling the Error field.
	Error    *ErrJsonRpcExceptionExternal
	errBytes []byte
	errMu    sync.RWMutex

	// result is the raw bytes of the result from the response, and is used when writing responses.
	// Ideally we don't need to parse these bytes. In cases where we need a specific field (e.g. blockNumber)
	// we use Sonic library to traverse directly to target such field (vs marshalling the whole result in memory).
	result       []byte
	resultWriter util.ByteWriter
	resultMu     sync.RWMutex
	cachedNode   *ast.Node

	// canonicalHashWithIgnored caches hashes computed with ignored fields
	// Key is the pointer to the first element of the ignoreFields slice (if non-empty)
	// This works because consensus policy reuses the same slice for the same ignore patterns
	canonicalHashWithIgnored sync.Map
}

func (r *JsonRpcResponse) SetResult(result []byte) {
	r.resultMu.Lock()
	defer r.resultMu.Unlock()
	r.result = result
}

func (r *JsonRpcResponse) GetResultBytes() []byte {
	r.resultMu.RLock()
	defer r.resultMu.RUnlock()
	return r.result
}

func (r *JsonRpcResponse) GetResultString() string {
	return util.B2Str(r.GetResultBytes())
}

func (r *JsonRpcResponse) ResultLength() int {
	r.resultMu.RLock()
	ln := len(r.result)
	rw := r.resultWriter
	r.resultMu.RUnlock()
	if ln > 0 {
		return ln
	}
	if rw != nil {
		if sz, err := rw.Size(); err == nil {
			return sz
		}
	}
	return 0
}

// Free releases heavy, memory-retaining fields so that upstream response buffers
// can be garbage collected as soon as the response is no longer needed.
//
// This method is safe to call after the response has been fully consumed
// (written to client or converted by callers). It must not be used while
// there are concurrent readers of this object.
func (r *JsonRpcResponse) Free() {
	if r == nil {
		return
	}

	// Clear ID bytes
	r.idMu.Lock()
	r.idBytes = nil
	// keep r.id as is for logging if needed; it's tiny
	r.idMu.Unlock()

	// Clear error bytes
	r.errMu.Lock()
	r.errBytes = nil
	// keep r.Error reference small
	r.errMu.Unlock()

	r.resultMu.Lock()
	// Release the resultWriter if it implements ReleasableByteWriter
	if r.resultWriter != nil {
		if releasable, ok := r.resultWriter.(util.ReleasableByteWriter); ok {
			releasable.Release()
		}
	}
	r.resultWriter = nil
	r.cachedNode = nil
	r.result = nil
	r.resultMu.Unlock()

	// Clear canonical hash caches
	r.canonicalHashWithIgnored.Range(func(key, _ any) bool {
		r.canonicalHashWithIgnored.Delete(key)
		return true
	})
}

func NewJsonRpcResponse(id interface{}, result interface{}, rpcError *ErrJsonRpcExceptionExternal) (*JsonRpcResponse, error) {
	resultRaw, err := SonicCfg.Marshal(result)
	if err != nil {
		return nil, err
	}
	idBytes, err := SonicCfg.Marshal(id)
	if err != nil {
		return nil, err
	}
	return &JsonRpcResponse{
		id:      id,
		idBytes: idBytes,
		result:  resultRaw,
		Error:   rpcError,
	}, nil
}

func MustNewJsonRpcResponse(id interface{}, result interface{}, rpcError *ErrJsonRpcExceptionExternal) *JsonRpcResponse {
	jr, err := NewJsonRpcResponse(id, result, rpcError)
	if err != nil {
		panic(err)
	}
	return jr
}

func NewJsonRpcResponseFromBytes(id []byte, resultRaw []byte, errBytes []byte) (*JsonRpcResponse, error) {
	jr := &JsonRpcResponse{
		idBytes: id,
		result:  resultRaw,
	}

	if len(errBytes) > 0 {
		// Copy to avoid retaining buffer via unsafe conversion
		err := jr.ParseError(string(errBytes))
		if err != nil {
			return nil, err
		}
	}

	if len(id) > 0 {
		err := jr.parseID()
		if err != nil {
			return nil, err
		}
	}

	return jr, nil
}

func MustNewJsonRpcResponseFromBytes(id []byte, resultRaw []byte, errBytes []byte) *JsonRpcResponse {
	jr, err := NewJsonRpcResponseFromBytes(id, resultRaw, errBytes)
	if err != nil {
		panic(err)
	}
	return jr
}

func (r *JsonRpcResponse) parseID() error {
	r.idMu.Lock()
	defer r.idMu.Unlock()

	var rawID interface{}
	err := SonicCfg.Unmarshal(r.idBytes, &rawID)
	if err != nil {
		return err
	}

	switch v := rawID.(type) {
	case float64:
		r.id = int64(v)
		// Update idBytes with the parsed int64 value
		r.idBytes, err = SonicCfg.Marshal(r.id)
		return err
	case string:
		r.id = v
		return nil
	case nil:
		return nil
	default:
		return fmt.Errorf("unsupported ID type: %T", v)
	}
}

func (r *JsonRpcResponse) SetID(id interface{}) error {
	r.idMu.Lock()
	defer r.idMu.Unlock()

	r.id = id
	var err error
	r.idBytes, err = SonicCfg.Marshal(id)
	if err != nil {
		return err
	}

	return nil
}

func (r *JsonRpcResponse) ID() interface{} {
	r.idMu.RLock()

	if r.id != nil {
		r.idMu.RUnlock()
		return r.id
	}
	r.idMu.RUnlock()

	r.idMu.Lock()
	defer r.idMu.Unlock()

	if len(r.idBytes) == 0 {
		return nil
	}

	err := SonicCfg.Unmarshal(r.idBytes, &r.id)
	if err != nil {
		log.Error().Err(err).Bytes("idBytes", r.idBytes).Msg("failed to unmarshal JsonRpcResponse.ID")
	}

	return r.id
}

func (r *JsonRpcResponse) SetIDBytes(idBytes []byte) error {
	r.idMu.Lock()
	defer r.idMu.Unlock()

	r.idBytes = idBytes
	return r.parseID()
}

func (r *JsonRpcResponse) ParseFromStream(ctx []context.Context, reader io.Reader, expectedSize int) error {
	if len(ctx) > 0 {
		_, span := StartDetailSpan(ctx[0], "JsonRpcResponse.ParseFromStream")
		defer span.End()
	}

	data, returnBuf, err := util.ReadAll(reader, expectedSize)
	if err != nil {
		return err
	}
	// Return buffer after we're done parsing and copying what we need
	if returnBuf != nil {
		defer returnBuf()
	}

	// Parse into a temporary struct to extract fields without string conversion
	var temp struct {
		ID     json.RawMessage `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}

	// Use Sonic's Unmarshal which works directly with bytes
	if err := SonicCfg.Unmarshal(data, &temp); err != nil {
		// Must copy data before storing since we're returning the buffer
		dataCopy := make([]byte, len(data))
		copy(dataCopy, data)
		r.resultMu.Lock()
		r.result = dataCopy
		r.resultMu.Unlock()
		return err
	}

	// Copy parsed bytes since we're returning the buffer
	if len(temp.ID) > 0 {
		idCopy := make([]byte, len(temp.ID))
		copy(idCopy, temp.ID)
		r.idMu.Lock()
		r.idBytes = idCopy
		r.idMu.Unlock()
	}

	if len(temp.Result) > 0 {
		resultCopy := make([]byte, len(temp.Result))
		copy(resultCopy, temp.Result)
		r.resultMu.Lock()
		r.result = resultCopy
		r.resultMu.Unlock()
	}

	if len(temp.Error) > 0 {
		if err := r.ParseError(string(temp.Error)); err != nil {
			return err
		}
	}

	// Parse ID if needed
	if len(r.idBytes) > 0 {
		if err := r.parseID(); err != nil {
			return err
		}
	}

	return nil
}

func (r *JsonRpcResponse) SetResultWriter(w util.ByteWriter) {
	r.resultMu.Lock()
	defer r.resultMu.Unlock()
	r.resultWriter = w
}

func (r *JsonRpcResponse) ParseError(raw string) error {
	r.errMu.Lock()
	defer r.errMu.Unlock()

	r.errBytes = nil

	// First attempt to unmarshal the error as a typical JSON-RPC error
	var rpcErr ErrJsonRpcExceptionExternal
	if err := SonicCfg.UnmarshalFromString(raw, &rpcErr); err != nil {
		// Special case: check for non-standard error structures in the raw data
		if raw == "" || raw == "null" {
			r.Error = NewErrJsonRpcExceptionExternal(
				int(JsonRpcErrorServerSideException),
				"unexpected empty response from upstream endpoint",
				"",
			)
			return nil
		}
	}

	// Check if the error is well-formed and has necessary fields
	if rpcErr.Code != 0 || rpcErr.Message != "" {
		r.Error = &rpcErr
		r.errBytes = []byte(raw)
		return nil
	}

	// Handle further special cases
	// Special case #1: numeric "code", "message", and "data"
	sp1 := &struct {
		Code    int    `json:"code,omitempty"`
		Message string `json:"message,omitempty"`
		Data    string `json:"data,omitempty"`
	}{}
	if err := SonicCfg.UnmarshalFromString(raw, sp1); err == nil {
		if sp1.Code != 0 || sp1.Message != "" || sp1.Data != "" {
			r.Error = NewErrJsonRpcExceptionExternal(
				sp1.Code,
				sp1.Message,
				sp1.Data,
			)
			return nil
		}
	}

	// Special case #2: only "error" field as a string
	sp2 := &struct {
		Error string `json:"error"`
	}{}
	if err := SonicCfg.UnmarshalFromString(raw, sp2); err == nil && sp2.Error != "" {
		r.Error = NewErrJsonRpcExceptionExternal(
			int(JsonRpcErrorServerSideException),
			sp2.Error,
			"",
		)
		return nil
	}

	// If no match, treat the raw data as message string
	r.Error = NewErrJsonRpcExceptionExternal(
		int(JsonRpcErrorServerSideException),
		raw,
		"",
	)
	return nil
}

func (r *JsonRpcResponse) PeekStringByPath(ctx context.Context, path ...interface{}) (string, error) {
	_, span := StartDetailSpan(ctx, "JsonRpcResponse.PeekStringByPath")
	defer span.End()

	err := r.ensureCachedNode()
	if err != nil {
		return "", err
	}

	r.resultMu.Lock()
	defer r.resultMu.Unlock()
	n := r.cachedNode.GetByPath(path...)
	if n == nil {
		return "", fmt.Errorf("could not get '%s' from json-rpc response", path)
	}
	if n.Error() != "" {
		return "", fmt.Errorf("error getting '%s' from json-rpc response: %s", path, n.Error())
	}

	return n.String()
}

func (r *JsonRpcResponse) PeekBytesByPath(ctx context.Context, path ...interface{}) ([]byte, error) {
	_, span := StartDetailSpan(ctx, "JsonRpcResponse.PeekBytesByPath")
	defer span.End()

	err := r.ensureCachedNode()
	if err != nil {
		return nil, err
	}

	r.resultMu.Lock()
	defer r.resultMu.Unlock()
	n := r.cachedNode.GetByPath(path...)
	if n == nil {
		return nil, fmt.Errorf("could not get '%s' from json-rpc response", path)
	}
	if n.Error() != "" {
		return nil, fmt.Errorf("error getting '%s' from json-rpc response: %s", path, n.Error())
	}

	// Use Raw() method to get zero-copy access to the underlying JSON bytes
	rawStr, err := n.Raw()
	if err != nil {
		return nil, fmt.Errorf("error getting raw JSON from node: %s", err)
	}
	return []byte(rawStr), nil
}

func (r *JsonRpcResponse) Size(ctx ...context.Context) (int, error) {
	if r == nil {
		return 0, nil
	}

	r.resultMu.RLock()
	rl := len(r.result)
	hasResult := rl > 0
	rw := r.resultWriter
	r.resultMu.RUnlock()
	if hasResult {
		return rl, nil
	}

	if rw != nil {
		return rw.Size(ctx...)
	}
	return 0, nil
}

func (r *JsonRpcResponse) MarshalZerologObject(e *zerolog.Event) {
	if r == nil {
		return
	}

	r.errMu.RLock()
	defer r.errMu.RUnlock()
	r.resultMu.RLock()
	defer r.resultMu.RUnlock()

	e.Interface("id", r.ID()).Int("resultSize", len(r.result))

	if len(r.errBytes) > 0 {
		if IsSemiValidJson(r.errBytes) {
			e.RawJSON("error", r.errBytes)
		} else {
			e.Str("error", string(r.errBytes))
		}
	} else if r.Error != nil {
		e.Interface("error", r.Error)
	}

	if len(r.result) > 0 {
		if len(r.result) < 300*1024 {
			if IsSemiValidJson(r.result) {
				e.RawJSON("result", r.result)
			} else {
				e.Str("result", string(r.result))
			}
		} else {
			head := 150 * 1024
			tail := len(r.result) - head
			if tail < head {
				head = tail
			}
			e.Str("resultHead", string(r.result[:head])).Str("resultTail", string(r.result[tail:]))
		}
	} else if r.resultWriter != nil {
		e.Bool("resultWriterEmpty", r.resultWriter.IsResultEmptyish())
	}
}

func (r *JsonRpcResponse) ensureCachedNode() error {
	r.resultMu.RLock()
	needBuild := r.cachedNode == nil
	emptyResult := len(r.result) == 0
	rw := r.resultWriter
	r.resultMu.RUnlock()
	if needBuild && emptyResult && rw != nil {
		// Materialize from writer for AST-based reads
		r.resultMu.Lock()
		if len(r.result) == 0 && r.resultWriter != nil {
			var buf bytes.Buffer
			if _, err := r.resultWriter.WriteTo(&buf, false); err != nil {
				r.resultMu.Unlock()
				return err
			}
			r.result = buf.Bytes()
			// Drop writer after materialization
			r.resultWriter = nil
		}
		r.resultMu.Unlock()
	}

	r.resultMu.RLock()
	if r.cachedNode == nil {
		srchr := ast.NewSearcher(util.B2Str(r.result))
		srchr.ValidateJSON = false
		srchr.ConcurrentRead = false
		srchr.CopyReturn = false
		n, err := srchr.GetByPath()
		if err != nil {
			r.resultMu.RUnlock()
			return err
		}
		r.resultMu.RUnlock()
		r.resultMu.Lock()
		// Copy 'n' onto the heap to avoid taking the address of a stack variable.
		// This prevents 'r.cachedNode' from pointing to memory that becomes invalid
		// once the function returns.
		r.cachedNode = new(ast.Node)
		*r.cachedNode = n
		r.resultMu.Unlock()
		return nil
	}

	r.resultMu.RUnlock()
	return nil
}

// MarshalJSON must not be used as it requires marshalling the whole response into a buffer in memory.
func (r *JsonRpcResponse) MarshalJSON() ([]byte, error) {
	return nil, fmt.Errorf("MarshalJSON must not be used on JsonRpcResponse")
}

// WriteTo implements io.WriterTo interface for JsonRpcResponse and is a custom implementation of marshalling json-rpc response,
// this approach uses minimum memory allocations and is faster than a generic JSON marshaller.
func (r *JsonRpcResponse) WriteTo(w io.Writer) (n int64, err error) {
	r.idMu.RLock()
	defer r.idMu.RUnlock()
	r.errMu.RLock()
	defer r.errMu.RUnlock()
	r.resultMu.RLock()
	defer r.resultMu.RUnlock()

	// Write the response opening
	nn, err := w.Write([]byte(`{"jsonrpc":"2.0","id":`))
	if err != nil {
		return int64(nn), err
	}
	n += int64(nn)

	// Write ID
	nn, err = w.Write(r.idBytes)
	if err != nil {
		return n + int64(nn), err
	}
	n += int64(nn)

	if len(r.errBytes) > 0 {
		// Write error field
		nn, err = w.Write([]byte(`,"error":`))
		if err != nil {
			return n + int64(nn), err
		}
		n += int64(nn)

		nn, err = w.Write(r.errBytes)
		if err != nil {
			return n + int64(nn), err
		}
		n += int64(nn)
	} else if r.Error != nil {
		// Marshal error on demand if errBytes not set
		r.errBytes, err = SonicCfg.Marshal(r.Error)
		if err != nil {
			return n, err
		}

		nn, err = w.Write([]byte(`,"error":`))
		if err != nil {
			return n + int64(nn), err
		}
		n += int64(nn)

		nn, err = w.Write(r.errBytes)
		if err != nil {
			return n + int64(nn), err
		}
		n += int64(nn)
	}

	if len(r.result) > 0 {
		// Write result field
		nn, err = w.Write([]byte(`,"result":`))
		if err != nil {
			return n + int64(nn), err
		}
		n += int64(nn)

		nn, err = w.Write(r.result)
		if err != nil {
			return n + int64(nn), err
		}
		n += int64(nn)
	} else if r.resultWriter != nil {
		// Write result field
		nn, err = w.Write([]byte(`,"result":`))
		if err != nil {
			return n + int64(nn), err
		}
		n += int64(nn)

		nw, err := r.resultWriter.WriteTo(w, false)
		if err != nil {
			return n + nw, err
		}
		n += nw
	}

	// Write closing brace
	nn, err = w.Write([]byte{'}'})
	return n + int64(nn), err
}

func (r *JsonRpcResponse) WriteResultTo(w io.Writer, trimSides bool) (n int64, err error) {
	r.resultMu.RLock()
	defer r.resultMu.RUnlock()

	if len(r.result) > 0 {
		if trimSides {
			if len(r.result) <= 2 {
				return 0, nil
			}
			nn, err := w.Write(r.result[1 : len(r.result)-1])
			return int64(nn), err
		}
		nn, err := w.Write(r.result)
		return int64(nn), err
	}

	return r.resultWriter.WriteTo(w, trimSides)
}

func (r *JsonRpcResponse) Clone() (*JsonRpcResponse, error) {
	if r == nil {
		return nil, nil
	}

	r.idMu.RLock()
	defer r.idMu.RUnlock()
	r.errMu.RLock()
	defer r.errMu.RUnlock()
	r.resultMu.RLock()
	defer r.resultMu.RUnlock()

	clone := &JsonRpcResponse{
		id:    r.id,
		Error: r.Error,
		// Do NOT copy cachedNode to avoid retaining original upstream buffer via AST.
		// It will be lazily rebuilt from clone.Result if needed.
		cachedNode: nil,
	}

	// Deep copy byte slices to avoid shared references
	if r.idBytes != nil {
		clone.idBytes = make([]byte, len(r.idBytes))
		copy(clone.idBytes, r.idBytes)
	}

	if r.errBytes != nil {
		clone.errBytes = make([]byte, len(r.errBytes))
		copy(clone.errBytes, r.errBytes)
	}

	if r.result != nil {
		clone.result = make([]byte, len(r.result))
		copy(clone.result, r.result)
	} else if r.resultWriter != nil {
		// If we have a resultWriter but no materialized result,
		// materialize it now to avoid sharing the writer instance
		// which could lead to data races or premature cleanup
		var buf bytes.Buffer
		_, err := r.resultWriter.WriteTo(&buf, false)
		if err != nil {
			return nil, err
		}
		clone.result = buf.Bytes()
		// Don't copy the resultWriter since we've materialized the result
		clone.resultWriter = nil
	}

	// Copy the canonical hash if it exists
	if cached, ok := r.canonicalHashWithIgnored.Load(defaultCanonicalHashPlaceholder); ok {
		clone.canonicalHashWithIgnored.Store(defaultCanonicalHashPlaceholder, cached)
	}

	// Only copy resultWriter if we didn't materialize the result above
	if clone.result == nil && r.resultWriter != nil {
		clone.resultWriter = r.resultWriter
	}

	return clone, nil
}

func (r *JsonRpcResponse) IsResultEmptyish(ctx ...context.Context) bool {
	var span trace.Span
	if len(ctx) > 0 {
		_, span = StartDetailSpan(ctx[0], "JsonRpcResponse.IsResultEmptyish")
		defer span.End()
	}

	if r == nil {
		if span != nil {
			span.SetAttributes(attribute.Bool("resultEmptyish", true))
		}
		return true
	}

	r.resultMu.RLock()
	defer r.resultMu.RUnlock()

	if len(r.result) > 0 {
		e := util.IsBytesEmptyish(r.result)
		if span != nil {
			span.SetAttributes(attribute.Bool("resultEmptyish", e))
		}
		return e
	}

	if r.resultWriter != nil {
		e := r.resultWriter.IsResultEmptyish()
		if span != nil {
			span.SetAttributes(attribute.Bool("resultEmptyish", e))
		}
		return e
	}

	if span != nil {
		span.SetAttributes(attribute.Bool("resultEmptyish", true))
	}
	return true
}

func (r *JsonRpcResponse) CanonicalHash(ctx ...context.Context) (string, error) {
	if r == nil {
		return "", nil
	}

	// Fast-path: if already computed, return immediately
	if cached, ok := r.canonicalHashWithIgnored.Load(defaultCanonicalHashPlaceholder); ok {
		return cached.(string), nil
	}

	// Compute hash
	r.resultMu.RLock()
	resultCopy := r.result
	r.resultMu.RUnlock()

	var obj interface{}
	if err := SonicCfg.Unmarshal(resultCopy, &obj); err != nil {
		return "", err
	}

	h := sha256.New()
	_, err := canonicalizeTo(h, obj)
	if err != nil {
		return "", err
	}
	hash := hex.EncodeToString(h.Sum(nil))

	// Store the computed hash atomically
	r.canonicalHashWithIgnored.Store(defaultCanonicalHashPlaceholder, hash)

	return hash, nil
}

// CanonicalHashWithIgnoredFields calculates the canonical hash while ignoring specified field paths
func (r *JsonRpcResponse) CanonicalHashWithIgnoredFields(ignoreFields []string, ctx ...context.Context) (string, error) {
	if r == nil {
		return "", nil
	}

	// If no fields to ignore, use regular canonical hash
	if len(ignoreFields) == 0 {
		return r.CanonicalHash(ctx...)
	}

	// Use the slice's underlying array pointer as cache key
	// This is safe because consensus policy reuses the same slice for the same ignore patterns
	var cacheKey unsafe.Pointer
	if len(ignoreFields) > 0 {
		cacheKey = unsafe.Pointer(&ignoreFields[0]) // #nosec G103
	}

	// Check cache first
	if cached, ok := r.canonicalHashWithIgnored.Load(cacheKey); ok {
		return cached.(string), nil
	}

	// Compute hash with ignored fields
	r.resultMu.RLock()
	resultCopy := r.result
	r.resultMu.RUnlock()

	var obj interface{}
	if err := SonicCfg.Unmarshal(resultCopy, &obj); err != nil {
		return "", err
	}

	// Remove ignored fields before canonicalization
	obj = removeFieldsByPaths(obj, ignoreFields)

	h := sha256.New()
	_, err := canonicalizeTo(h, obj)
	if err != nil {
		return "", err
	}
	hash := hex.EncodeToString(h.Sum(nil))

	// Store in cache
	r.canonicalHashWithIgnored.Store(cacheKey, hash)

	return hash, nil
}

// canonicalizeTo writes the canonical JSON representation of v into w, applying
// the same emptyish filtering semantics as canonicalize(). It returns true if
// any bytes were written.
func canonicalizeTo(w io.Writer, v interface{}) (bool, error) {
	if isEmptyishValue(v) {
		return false, nil
	}

	switch val := v.(type) {
	case map[string]interface{}:
		// Filter empties and precompute children
		keys := make([]string, 0, len(val))
		children := make(map[string]*bytes.Buffer, len(val))
		for k, vv := range val {
			if isEmptyishValue(vv) {
				continue
			}
			buf := util.BorrowBuf()
			wrote, err := canonicalizeTo(buf, vv)
			if err != nil {
				util.ReturnBuf(buf)
				return false, err
			}
			if wrote {
				children[k] = buf
				keys = append(keys, k)
			} else {
				util.ReturnBuf(buf)
			}
		}
		if len(keys) == 0 {
			return false, nil
		}
		sort.Strings(keys)
		// Ensure all borrowed child buffers are returned to the pool even if any write below errors.
		defer func() {
			for _, cb := range children {
				if cb != nil {
					util.ReturnBuf(cb)
				}
			}
		}()
		if _, err := w.Write([]byte{'{'}); err != nil {
			return false, err
		}
		for i, k := range keys {
			if i > 0 {
				if _, err := w.Write([]byte{','}); err != nil {
					return false, err
				}
			}
			kj, _ := SonicCfg.Marshal(k)
			if _, err := w.Write(kj); err != nil {
				return false, err
			}
			if _, err := w.Write([]byte{':'}); err != nil {
				return false, err
			}
			if _, err := w.Write(children[k].Bytes()); err != nil {
				return false, err
			}
			// Return immediately and mark as nil so deferred cleanup skips it
			util.ReturnBuf(children[k])
			children[k] = nil
		}
		_, err := w.Write([]byte{'}'})
		return true, err

	case []interface{}:
		// Filter empties and stream elements
		if _, err := w.Write([]byte{'['}); err != nil {
			return false, err
		}
		first := true
		any := false
		for _, item := range val {
			if isEmptyishValue(item) {
				continue
			}
			buf := util.BorrowBuf()
			wrote, err := canonicalizeTo(buf, item)
			if err != nil {
				util.ReturnBuf(buf)
				return false, err
			}
			if !wrote {
				util.ReturnBuf(buf)
				continue
			}
			if !first {
				if _, err := w.Write([]byte{','}); err != nil {
					util.ReturnBuf(buf)
					return false, err
				}
			}
			first = false
			any = true
			if _, err := w.Write(buf.Bytes()); err != nil {
				util.ReturnBuf(buf)
				return false, err
			}
			util.ReturnBuf(buf)
		}
		if _, err := w.Write([]byte{']'}); err != nil {
			return false, err
		}
		if !any {
			return false, nil
		}
		return true, nil

	case string:
		b := removeLeadingZeroes(util.S2Bytes(val))
		if len(b) == 0 {
			return false, nil
		}
		_, err := w.Write(b)
		return true, err

	case []byte:
		b := removeLeadingZeroes(val)
		if len(b) == 0 {
			return false, nil
		}
		_, err := w.Write(b)
		return true, err

	default:
		b, err := SonicCfg.Marshal(val)
		if err != nil {
			return false, err
		}
		if util.IsBytesEmptyish(b) {
			return false, nil
		}
		b = removeLeadingZeroes(b)
		_, err = w.Write(b)
		return true, err
	}
}

// removeFieldsByPaths removes fields from an object based on dot-separated paths
// Supports:
// - Simple fields: "status"
// - Nested fields: "receipt.status"
// - Array wildcards: "logs.*.blockTimestamp"
func removeFieldsByPaths(obj interface{}, paths []string) interface{} {
	if obj == nil || len(paths) == 0 {
		return obj
	}

	// Parse paths into a tree structure for efficient removal
	pathTree := make(map[string]interface{})
	for _, path := range paths {
		parts := strings.Split(path, ".")
		current := pathTree
		for i, part := range parts {
			if i == len(parts)-1 {
				current[part] = true // Leaf node
			} else {
				if _, exists := current[part]; !exists {
					current[part] = make(map[string]interface{})
				}
				if next, ok := current[part].(map[string]interface{}); ok {
					current = next
				}
			}
		}
	}

	return removeFieldsRecursive(obj, pathTree)
}

// removeFieldsRecursive recursively removes fields based on the path tree
func removeFieldsRecursive(obj interface{}, pathTree map[string]interface{}) interface{} {
	switch v := obj.(type) {
	case map[string]interface{}:
		result := make(map[string]interface{})
		for key, value := range v {
			// Check if this key should be removed or has nested removals
			if pathNode, exists := pathTree[key]; exists {
				if pathNode == true {
					// This field should be removed entirely
					continue
				} else if nestedTree, ok := pathNode.(map[string]interface{}); ok {
					// Apply nested removals
					result[key] = removeFieldsRecursive(value, nestedTree)
				}
			} else if wildcardTree, exists := pathTree["*"]; exists {
				// Handle wildcard for arrays
				if nestedTree, ok := wildcardTree.(map[string]interface{}); ok {
					result[key] = removeFieldsRecursive(value, nestedTree)
				}
			} else {
				// Keep the field as-is
				result[key] = value
			}
		}
		return result

	case []interface{}:
		// Handle array wildcards
		if wildcardTree, exists := pathTree["*"]; exists {
			if nestedTree, ok := wildcardTree.(map[string]interface{}); ok {
				result := make([]interface{}, len(v))
				for i, item := range v {
					result[i] = removeFieldsRecursive(item, nestedTree)
				}
				return result
			}
		}
		return v

	default:
		// Primitive types or other types are returned as-is
		return v
	}
}

func removeLeadingZeroes(b []byte) []byte {
	if len(b) > 2 && b[0] == '0' && (b[1] == 'x' || b[1] == 'X') {
		b = bytes.TrimLeft(b[2:], "0")
	} else if len(b) > 3 && b[0] == '"' && b[1] == '0' && b[2] == 'x' && b[3] == '0' {
		b = bytes.TrimLeft(b[3:], "0")
		if len(b) == 1 {
			return nil
		}
	}
	return b
}

// isEmptyishValue checks if a value should be considered empty for canonicalization
func isEmptyishValue(v interface{}) bool {
	if v == nil {
		return true
	}

	switch val := v.(type) {
	case string:
		// Empty string
		if val == "" {
			return true
		}
		// Check for hex values that are all zeros (0x, 0x0, 0x00, 0x000, etc.)
		if strings.HasPrefix(val, "0x") {
			// Remove the 0x prefix and check if the rest is all zeros
			hexPart := val[2:]
			if hexPart == "" {
				return true // "0x" is empty
			}
			// Check if all remaining characters are zeros
			for _, c := range hexPart {
				if c != '0' {
					return false
				}
			}
			return true // All zeros after 0x
		}
		return false
	case []interface{}:
		// Empty arrays are considered empty
		return len(val) == 0
	case map[string]interface{}:
		// Empty objects are considered empty
		return len(val) == 0
	case float64:
		return val == 0
	case bool:
		// Booleans are never emptyish
		return false
	default:
		// For other types, marshal and check
		b, err := SonicCfg.Marshal(val)
		if err != nil {
			return false
		}
		return util.IsBytesEmptyish(b)
	}
}

//
//
// JSON-RPC Request
//

type JsonRpcRequest struct {
	sync.RWMutex

	JSONRPC string        `json:"jsonrpc,omitempty"`
	ID      interface{}   `json:"id,omitempty"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`

	cacheHash atomic.Value
}

func NewJsonRpcRequest(method string, params []interface{}) *JsonRpcRequest {
	return &JsonRpcRequest{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	}
}

func (r *JsonRpcRequest) LockWithTrace(ctx context.Context) {
	_, span := StartDetailSpan(ctx, "JsonRpcRequest.Lock")
	defer span.End()
	r.Lock()
}

func (r *JsonRpcRequest) RLockWithTrace(ctx context.Context) {
	_, span := StartDetailSpan(ctx, "JsonRpcRequest.RLock")
	defer span.End()
	r.RLock()
}

func (r *JsonRpcRequest) Clone() *JsonRpcRequest {
	r.RLock()
	defer r.RUnlock()

	// Deep copy the params to avoid concurrent access issues
	var clonedParams []interface{}
	if r.Params != nil {
		clonedParams = make([]interface{}, len(r.Params))
		for i, param := range r.Params {
			clonedParams[i] = deepCopyValue(param)
		}
	}

	return &JsonRpcRequest{
		JSONRPC: r.JSONRPC,
		ID:      r.ID,
		Method:  r.Method,
		Params:  clonedParams,
	}
}

// deepCopyValue creates a deep copy of a value to avoid concurrent access issues
func deepCopyValue(v interface{}) interface{} {
	if v == nil {
		return nil
	}

	switch val := v.(type) {
	case map[string]interface{}:
		// Deep copy map
		newMap := make(map[string]interface{}, len(val))
		for k, v := range val {
			newMap[k] = deepCopyValue(v)
		}
		return newMap
	case []interface{}:
		// Deep copy slice
		newSlice := make([]interface{}, len(val))
		for i, v := range val {
			newSlice[i] = deepCopyValue(v)
		}
		return newSlice
	default:
		// Primitive types are safe to copy directly
		return val
	}
}

func (r *JsonRpcRequest) SetID(id interface{}) error {
	r.Lock()
	defer r.Unlock()

	r.ID = id
	return nil
}

func (r *JsonRpcRequest) UnmarshalJSON(data []byte) error {
	type Alias JsonRpcRequest
	aux := &struct {
		*Alias
		ID json.RawMessage `json:"id,omitempty"`
	}{
		Alias: (*Alias)(r),
	}
	aux.JSONRPC = "2.0"

	if err := SonicCfg.Unmarshal(data, &aux); err != nil {
		return err
	}

	if aux.ID != nil {
		var id interface{}
		if err := SonicCfg.Unmarshal(aux.ID, &id); err != nil {
			return err
		}
		switch v := id.(type) {
		case float64:
			r.ID = int64(v)
		case string:
			r.ID = v
		}
	}

	if r.ID == nil {
		r.ID = util.RandomID()
	} else {
		switch r.ID.(type) {
		case string:
			if r.ID.(string) == "" {
				r.ID = util.RandomID()
			}
		}
	}

	return nil
}

func (r *JsonRpcRequest) MarshalZerologObject(e *zerolog.Event) {
	if r == nil {
		return
	}

	r.RLock()
	defer r.RUnlock()

	e.Str("method", r.Method).
		Interface("params", r.Params).
		Interface("id", r.ID)
}

func (r *JsonRpcRequest) CacheHash(ctx ...context.Context) (string, error) {
	if len(ctx) > 0 {
		_, span := StartDetailSpan(ctx[0], "Request.GenerateCacheHash")
		defer span.End()
	}
	if r == nil {
		return "", nil
	}

	if ch := r.cacheHash.Load(); ch != nil {
		return ch.(string), nil
	}

	hasher := sha256.New()
	for _, p := range r.Params {
		err := hashValue(hasher, p)
		if err != nil {
			return "", err
		}
	}
	b := sha256.Sum256(hasher.Sum(nil))
	ch := fmt.Sprintf("%s:%x", r.Method, b)
	r.cacheHash.Store(ch)
	return ch, nil
}

func (r *JsonRpcRequest) PeekByPath(path ...interface{}) (interface{}, error) {
	if r == nil || len(r.Params) == 0 {
		return nil, fmt.Errorf("cannot peek path on empty params")
	}

	// Start with params array as current value
	var current interface{} = r.Params

	// Traverse through path elements
	for _, p := range path {
		switch idx := p.(type) {
		case int:
			// Array index access
			arr, ok := current.([]interface{})
			if !ok {
				return nil, fmt.Errorf("expected array at path element %v, got %T", p, current)
			}
			if idx < 0 || idx >= len(arr) {
				return nil, fmt.Errorf("array index %d out of bounds, array length: %d", idx, len(arr))
			}
			current = arr[idx]

		case string:
			// Map key access
			m, ok := current.(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("expected map at path element %v, got %T", p, current)
			}
			val, exists := m[idx]
			if !exists {
				return nil, fmt.Errorf("key %s not found in map %+v", idx, m)
			}
			current = val

		default:
			return nil, fmt.Errorf("unsupported path element type: %T", p)
		}
	}

	return current, nil
}

func hashValue(h io.Writer, v interface{}) error {
	switch t := v.(type) {
	case bool:
		_, err := io.WriteString(h, fmt.Sprintf("%t", t))
		return err
	case int:
		_, err := io.WriteString(h, fmt.Sprintf("%d", t))
		return err
	case float64:
		_, err := io.WriteString(h, fmt.Sprintf("%f", t))
		return err
	case string:
		_, err := io.WriteString(h, strings.ToLower(t))
		return err
	case []interface{}:
		for _, i := range t {
			err := hashValue(h, i)
			if err != nil {
				return err
			}
		}
		return nil
	case map[string]interface{}:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if _, err := h.Write([]byte(k)); err != nil {
				return err
			}
			err := hashValue(h, t[k])
			if err != nil {
				return err
			}
		}
		return nil
	case nil:
		_, err := h.Write([]byte("null"))
		return err
	default:
		return fmt.Errorf("unsupported type for value during hash: %+v", v)
	}
}

// TranslateToJsonRpcException is mainly responsible to translate internal eRPC errors (not those coming from upstreams) to
// a proper json-rpc error with correct numeric code.
func TranslateToJsonRpcException(err error) error {
	if erx, ok := err.(*ErrUpstreamsExhausted); ok {
		// Scan an UpstreamsExhausted error to detect the most frequent error among upstreams
		// This selection helps provide somewhat user-friendly error message vs just saying that "all upstreams failed"
		var (
			counts   = make(map[ErrorCode]int) // code -> occurrences
			maxCount int
			domErr   error
		)
		for _, cause := range erx.Errors() {
			se, ok := cause.(StandardError)
			if !ok {
				continue
			}
			c := se.Base().Code
			if counts[c] == 0 {
				// keep the first error for each code so we don't have to iterate again later
				// we store it only the first time we see the code which guarantees we return the earliest error
				if domErr == nil { // fast path when first iteration becomes dominant by default
					domErr = cause
				}
			}
			if c == ErrCodeUpstreamRequestSkipped || HasErrorCode(cause, ErrCodeEndpointUnsupported) {
				// most often these errors are not interesting nor significant vs other errors
				continue
			}
			counts[c]++
			if counts[c] > maxCount {
				maxCount = counts[c]
				domErr = cause // we want the first error with the dominant code; since we update only when count is strictly greater, the earliest error for that code is kept in domErr.
			}
		}
		if domErr != nil {
			err = domErr
		}
	}

	// If the error already has a json-rpc representation in the chain, return it as is
	// e.g. when it is based on response from upstream
	if HasErrorCode(err, ErrCodeJsonRpcExceptionInternal) {
		return err
	}

	//
	// When error is an internal eRPC error, we need to translate it to a json-rpc error
	//
	if HasErrorCode(
		err,
		ErrCodeAuthRateLimitRuleExceeded,
		ErrCodeProjectRateLimitRuleExceeded,
		ErrCodeNetworkRateLimitRuleExceeded,
		ErrCodeUpstreamRateLimitRuleExceeded,
	) {
		return NewErrJsonRpcExceptionInternal(
			0,
			JsonRpcErrorCapacityExceeded,
			"rate-limit exceeded",
			err,
			nil,
		)
	}
	if HasErrorCode(
		err,
		ErrCodeAuthUnauthorized,
	) {
		return NewErrJsonRpcExceptionInternal(
			0,
			JsonRpcErrorUnauthorized,
			"unauthorized",
			err,
			nil,
		)
	}
	if HasErrorCode(err, ErrCodeUpstreamMethodIgnored) {
		return NewErrJsonRpcExceptionInternal(
			0,
			JsonRpcErrorUnsupportedException,
			"method ignored by upstream",
			err,
			nil,
		)
	}
	if HasErrorCode(err, ErrCodeJsonRpcRequestUnmarshal) {
		return NewErrJsonRpcExceptionInternal(
			0,
			JsonRpcErrorParseException,
			"failed to parse json-rpc request",
			err,
			nil,
		)
	}
	if HasErrorCode(err, ErrCodeInvalidRequest, ErrCodeInvalidUrlPath) {
		return NewErrJsonRpcExceptionInternal(
			0,
			JsonRpcErrorInvalidArgument,
			"invalid request url and/or body",
			err,
			nil,
		)
	}
	if HasErrorCode(err, ErrCodeGetLogsExceededMaxAllowedRange, ErrCodeGetLogsExceededMaxAllowedAddresses, ErrCodeGetLogsExceededMaxAllowedTopics) {
		return NewErrJsonRpcExceptionInternal(
			0,
			JsonRpcErrorEvmLargeRange,
			"getLogs request exceeded max allowed range",
			err,
			nil,
		)
	}
	var msg = "internal server error"
	if se, ok := err.(StandardError); ok {
		msg = se.DeepestMessage()
	} else {
		msg = err.Error()
	}

	return NewErrJsonRpcExceptionInternal(
		0,
		JsonRpcErrorServerSideException,
		msg,
		err,
		nil,
	)
}
