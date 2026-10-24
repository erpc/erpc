// The realworld suite builds cmd/erpc with this file added (go build
// -overlay); release builds never see it. REALWORLD_VARS=name=duration,…
// stores each named duration, as in-process tests Store them, so a phase
// can shorten a bound the spec states in minutes.
package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/erpc/erpc/architecture/evm"
	"github.com/erpc/erpc/indexer/adapters/wsupstream"
	"github.com/erpc/erpc/util"
)

var realworldVars = map[string]*util.Duration{
	"evm.LatestBlockSkipExpiry":     evm.LatestBlockSkipExpiry,
	"wsupstream.HeadsReaskInterval": wsupstream.HeadsReaskInterval,
}

func init() {
	env := os.Getenv("REALWORLD_VARS")
	if env == "" {
		return
	}
	for _, kv := range strings.Split(env, ",") {
		name, val, _ := strings.Cut(kv, "=")
		v, ok := realworldVars[name]
		if !ok {
			panic(fmt.Sprintf("REALWORLD_VARS names %q, no variable", name))
		}
		d, err := time.ParseDuration(val)
		if err != nil {
			panic(fmt.Sprintf("REALWORLD_VARS %s: %v", name, err))
		}
		v.Store(d)
	}
}
