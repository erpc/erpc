package test

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"sync"
	"time"
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

type FakeServer struct {
	Port            int
	FailureRate     float64
	MinDelay        time.Duration
	MaxDelay        time.Duration
	server          *http.Server
	mu              sync.Mutex
	requestsHandled int64
}

func NewFakeServer(port int, failureRate float64, minDelay, maxDelay time.Duration) *FakeServer {
	return &FakeServer{
		Port:        port,
		FailureRate: failureRate,
		MinDelay:    minDelay,
		MaxDelay:    maxDelay,
	}
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

	// Simulate delay
	time.Sleep(fs.randomDelay())

	// Decode JSON-RPC request
	var req JSONRPCRequest
	err := json.NewDecoder(r.Body).Decode(&req)
	if err != nil {
		http.Error(w, "Invalid JSON-RPC request", http.StatusBadRequest)
		return
	}

	// Simulate failure based on failure rate
	if rand.Float64() < fs.FailureRate {
		response := JSONRPCResponse{
			Jsonrpc: "2.0",
			Error:   "Simulated failure",
			ID:      req.ID,
		}
		json.NewEncoder(w).Encode(response)
		return
	}

	// Successful response
	response := JSONRPCResponse{
		Jsonrpc: "2.0",
		Result:  fmt.Sprintf("Processed method: %s", req.Method),
		ID:      req.ID,
	}
	json.NewEncoder(w).Encode(response)
}

func (fs *FakeServer) randomDelay() time.Duration {
	return fs.MinDelay + time.Duration(rand.Int63n(int64(fs.MaxDelay-fs.MinDelay)))
}

func (fs *FakeServer) RequestsHandled() int64 {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.requestsHandled
}