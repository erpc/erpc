package main

import (
	"github.com/rs/zerolog"
)

func init() {
	zerolog.SetGlobalLevel(zerolog.Disabled)
}
