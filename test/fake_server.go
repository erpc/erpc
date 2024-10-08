// #nosec
package test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bytedance/sonic"
)

type JSONRPCRequest struct {
	Jsonrpc string      `json:"jsonrpc"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
	ID      interface{} `json:"id"`
}

type JSONRPCResponse struct {
	Jsonrpc string      `json:"jsonrpc"`
	Result  interface{} `json:"result,omitempty"`
	Error   interface{} `json:"error,omitempty"`
	ID      interface{} `json:"id"`
}

type RequestResponseSample struct {
	Request  JSONRPCRequest  `json:"request"`
	Response JSONRPCResponse `json:"response"`
}

type FakeServer struct {
	Port            int
	FailureRate     float64
	LimitedRate     float64
	MinDelay        time.Duration
	MaxDelay        time.Duration
	server          *http.Server
	mu              sync.Mutex
	requestsHandled int64
	requestsFailed  int64
	requestsLimited int64
	requestsSuccess int64
	samples         []RequestResponseSample
}

func NewFakeServer(port int, failureRate float64, limitedRate float64, minDelay, maxDelay time.Duration, sampleFilePath string) (*FakeServer, error) {
	fs := &FakeServer{
		Port:        port,
		FailureRate: failureRate,
		LimitedRate: limitedRate,
		MinDelay:    minDelay,
		MaxDelay:    maxDelay,
	}

	if err := fs.loadSamples(sampleFilePath); err != nil {
		return nil, fmt.Errorf("failed to load samples: %w", err)
	}

	return fs, nil
}

func (fs *FakeServer) loadSamples(filePath string) error {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("failed to read sample file: %w", err)
	}

	if err := sonic.Unmarshal(data, &fs.samples); err != nil {
		return fmt.Errorf("failed to unmarshal samples: %w", err)
	}

	return nil
}

func (fs *FakeServer) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", fs.handleRequest)

	fs.server = &http.Server{
		Addr:    fmt.Sprintf(":%d", fs.Port),
		Handler: mux,
	}

	return fs.server.ListenAndServe()
}

func (fs *FakeServer) Stop() error {
	if fs.server != nil {
		return fs.server.Close()
	}
	return nil
}

func (fs *FakeServer) handleRequest(w http.ResponseWriter, r *http.Request) {
	fs.mu.Lock()
	fs.requestsHandled++
	fs.mu.Unlock()

	// Read the request body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		fs.sendErrorResponse(w, nil, -32700, "Parse error")
		return
	}

	// Try to unmarshal as a batch request
	var batchReq []JSONRPCRequest
	err = json.Unmarshal(body, &batchReq)
	if err == nil {
		// Handle batch request
		fs.handleBatchRequest(w, batchReq)
		return
	}

	// Try to unmarshal as a single request
	var singleReq JSONRPCRequest
	err = json.Unmarshal(body, &singleReq)
	if err == nil {
		// Handle single request
		fs.handleSingleRequest(w, singleReq)
		return
	}

	// Invalid JSON-RPC request
	fs.sendErrorResponse(w, nil, -32600, "Invalid Request")
}

func (fs *FakeServer) handleBatchRequest(w http.ResponseWriter, batchReq []JSONRPCRequest) {
	if len(batchReq) == 0 {
		// Empty batch request
		fs.sendErrorResponse(w, nil, -32600, "Invalid Request")
		return
	}

	responses := make([]JSONRPCResponse, 0, len(batchReq))
	for _, req := range batchReq {
		response := fs.processSingleRequest(req)
		if response != nil {
			response.ID = req.ID
			responses = append(responses, *response)
		}
	}

	if len(responses) > 0 {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(responses)
	}
}

func (fs *FakeServer) handleSingleRequest(w http.ResponseWriter, req JSONRPCRequest) {
	response := fs.processSingleRequest(req)
	if response != nil {
		w.Header().Set("Content-Type", "application/json")
		response.ID = req.ID

		buf := &bytes.Buffer{}
		if err := json.NewEncoder(buf).Encode(response); err != nil {
			fs.sendErrorResponse(w, req.ID, -32000, "Internal error")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(buf.Bytes())))
		w.Write(buf.Bytes())
	}
}

func (fs *FakeServer) processSingleRequest(req JSONRPCRequest) *JSONRPCResponse {
	// Validate request
	if req.Jsonrpc != "2.0" || req.Method == "" {
		fs.mu.Lock()
		fs.requestsFailed++
		fs.mu.Unlock()
		return &JSONRPCResponse{
			Jsonrpc: "2.0",
			Error:   map[string]interface{}{"code": -32600, "message": "Invalid Request"},
			ID:      req.ID,
		}
	}

	// Simulate delay
	time.Sleep(fs.randomDelay())

	// Simulate rate limiting
	if rand.Float64() < fs.LimitedRate {
		fs.mu.Lock()
		fs.requestsLimited++
		fs.mu.Unlock()
		if req.ID != nil {
			return &JSONRPCResponse{
				Jsonrpc: "2.0",
				Error:   map[string]interface{}{"code": -32000, "message": "simulated capacity exceeded"},
				ID:      req.ID,
			}
		}
		return nil // Notification, no response
	}

	// Simulate failure
	if rand.Float64() < fs.FailureRate {
		fs.mu.Lock()
		fs.requestsFailed++
		fs.mu.Unlock()
		if req.ID != nil {
			return &JSONRPCResponse{
				Jsonrpc: "2.0",
				Error:   map[string]interface{}{"code": -32001, "message": "simulated internal server failure"},
				ID:      req.ID,
			}
		}
		return nil // Notification, no response
	}

	fs.mu.Lock()
	fs.requestsSuccess++
	fs.mu.Unlock()

	// Find matching sample or use default response
	response := fs.findMatchingSample(req)
	if response == nil && req.ID != nil {
		response = &JSONRPCResponse{
			Jsonrpc: "2.0",
			Result:  fmt.Sprintf("Default response for method: %s", req.Method),
			ID:      req.ID,
		}
	}

	return response
}

func (fs *FakeServer) sendErrorResponse(w http.ResponseWriter, id interface{}, code int, message string) {
	fs.mu.Lock()
	fs.requestsFailed++
	fs.mu.Unlock()

	response := JSONRPCResponse{
		Jsonrpc: "2.0",
		Error:   map[string]interface{}{"code": code, "message": message},
		ID:      id,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (fs *FakeServer) findMatchingSample(req JSONRPCRequest) *JSONRPCResponse {
	for _, sample := range fs.samples {
		if sample.Request.Method == req.Method {
			// if params values are also equal one by one
			if len(sample.Request.Params.([]interface{})) == len(req.Params.([]interface{})) {
				for i, param := range sample.Request.Params.([]interface{}) {
					if param != req.Params.([]interface{})[i] {
						break
					}
				}
			}

			if txt, ok := sample.Response.Result.(string); ok {
				if strings.HasPrefix(txt, "bigfake:") {
					size, err := strconv.Atoi(txt[len("bigfake:"):])
					fmt.Printf("Generating big response of size %d\n", size)
					if err != nil {
						return nil
					}
					// generate a random string with the size of the number
					sample.Response.Result = make([]byte, size)
				}
			}

			return &sample.Response
		}
	}
	return nil
}

func (fs *FakeServer) randomDelay() time.Duration {
	return fs.MinDelay + time.Duration(rand.Int63n(int64(fs.MaxDelay-fs.MinDelay)))
}

func (fs *FakeServer) RequestsHandled() int64 {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.requestsHandled
}

func (fs *FakeServer) RequestsFailed() int64 {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.requestsFailed
}

func (fs *FakeServer) RequestsSuccess() int64 {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.requestsSuccess
}
