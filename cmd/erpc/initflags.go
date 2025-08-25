//go:build !test

package main

import (
	"io"
	"os"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func init() {
	// Honour ERPC_NOLOGS=1 to silence all zerolog output & allocations
	if os.Getenv("ERPC_NOLOGS") == "1" {
		zerolog.SetGlobalLevel(zerolog.Disabled)
		log.Logger = zerolog.New(io.Discard)
	}

	// Honour ERPC_NOMETRICS=1 to disable Prometheus metrics completely
	if os.Getenv("ERPC_NOMETRICS") == "1" {
		// Replace the default registry with an empty one,
		// so any metric creations succeed but never allocate label-pairs.
		r := prometheus.NewRegistry()
		prometheus.DefaultRegisterer = r
		prometheus.DefaultGatherer = r
	}
}
