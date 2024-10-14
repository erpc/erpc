package common

import (
	"github.com/rs/zerolog"
)

func init() {
	zerolog.SetGlobalLevel(zerolog.Disabled)
}
