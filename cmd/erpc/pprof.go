package main

import (
	"net/http"
	_ "net/http/pprof"
	"os"
	"runtime"

	"github.com/rs/zerolog/log"
)

// pprof is gated at RUNTIME on ERPC_PPROF=true rather than the old `pprof`
// build tag. The tag meant production binaries physically could not be
// profiled: when erpc's heap went 21% -> 64% in one minute during the
// 2026-08-05 consensus caller-abandonment pile-up (monad upstreams stopped
// answering; abandoned analyses accumulated until OOM), the ONE artifact that
// would have identified what held the memory — a heap profile — was impossible
// to capture, because the deployed image was a stock build without the tag.
//
// Runtime gating keeps the default posture identical (no listener, no
// overhead: the init returns before touching profiling state) while letting an
// operator flip an env var and restart to make the next incident diagnosable.
// The stdlib pprof import itself costs nothing when the server isn't running.
//
// The listener binds all interfaces, as before — reachable only within the
// task/pod network namespace unless a port mapping deliberately exposes it.
// SetMutexProfileFraction/SetBlockProfileRate stay inside the gate: both add
// sampling overhead and must not run in the default posture.
func init() {
	if os.Getenv("ERPC_PPROF") != "true" {
		return
	}
	go func() {
		runtime.SetMutexProfileFraction(1)
		runtime.SetBlockProfileRate(1)
		port := os.Getenv("ERPC_PPROF_PORT")
		if port == "" {
			port = "6060"
		}
		log.Info().Msgf("pprof server started at http://localhost:%s", port)
		if err := http.ListenAndServe("0.0.0.0:"+port, nil); err != nil {
			log.Error().Err(err).Msg("pprof server failed to start")
		}
	}()
}
