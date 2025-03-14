package common

import (
	"strings"

	"github.com/grafana/sobek"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// console provides a trivial implementation of console logging to
// aid in debugging user scripts.
func console(rt *sobek.Runtime) sobek.Value {
	// This function signature avoids reflection.
	return rt.ToValue(map[string]func(call sobek.FunctionCall) sobek.Value{
		"debug": func(call sobek.FunctionCall) sobek.Value { return consoleLog(zerolog.DebugLevel, call) },
		"info":  func(call sobek.FunctionCall) sobek.Value { return consoleLog(zerolog.InfoLevel, call) },
		"log":   func(call sobek.FunctionCall) sobek.Value { return consoleLog(zerolog.InfoLevel, call) },
		"trace": func(call sobek.FunctionCall) sobek.Value { return consoleLog(zerolog.TraceLevel, call) },
		"warn":  func(call sobek.FunctionCall) sobek.Value { return consoleLog(zerolog.WarnLevel, call) },
	})
}

func consoleLog(level zerolog.Level, call sobek.FunctionCall) sobek.Value {
	logger := log.Logger
	if logger.GetLevel() > level {
		return sobek.Undefined()
	}

	var sb strings.Builder
	for idx, arg := range call.Arguments {
		if idx > 0 {
			sb.WriteRune(' ')
		}
		sb.WriteString(arg.String())
	}
	logger.WithLevel(level).Msg(sb.String())

	return sobek.Undefined()
}
