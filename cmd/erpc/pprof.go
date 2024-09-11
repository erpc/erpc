//go:build pprof
// +build pprof

package main

import (
	"runtime"
	"net/http"
	_ "net/http/pprof"

	"github.com/rs/zerolog/log"
)

func init() {
	go func() {
		runtime.SetMutexProfileFraction(1)
		runtime.SetBlockProfileRate(1)
		log.Info().Msgf("pprof server started at http://localhost:6060")
		http.ListenAndServe("0.0.0.0:6060", nil)
	}()
}
