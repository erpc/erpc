package data

import "time"

// Timeout buffer constants used across the shared state system
const (
	// DefaultOperationBuffer is the default time reserved for operations after lock acquisition
	DefaultOperationBuffer = 10 * time.Second

	// PollOperationBuffer is the time reserved for operations in the state poller after lock acquisition
	PollOperationBuffer = 15 * time.Second

	// MinPollTimeout is the minimum timeout for a complete poll cycle
	MinPollTimeout = 30 * time.Second
)
