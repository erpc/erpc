//go:build pprof
// +build pprof

package main

import (
	"net/http"
	_ "net/http/pprof"
	"os"
	"runtime"

	"github.com/rs/zerolog/log"
)

func init() {
	go func() {
		runtime.SetMutexProfileFraction(1)
		runtime.SetBlockProfileRate(1)
		port := os.Getenv("ERPC_PPROF_PORT")
		if port == "" {
			port = "6060"
		}
		log.Info().Msgf("pprof server started at http://localhost:%s", port)
		http.ListenAndServe("0.0.0.0:"+port, nil)
	}()
}
