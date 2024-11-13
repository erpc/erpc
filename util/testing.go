package util

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/h2non/gock"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func IsTest() bool {
	return flag.Lookup("test.v") != nil
}

func ConfigureTestLogger() {
	zerolog.SetGlobalLevel(zerolog.Disabled)
	lvl, err := zerolog.ParseLevel(os.Getenv("LOG_LEVEL"))
	if err == nil && lvl != zerolog.NoLevel {
		zerolog.SetGlobalLevel(lvl)
	} else if err != nil {
		fmt.Println("LOG_LEVEL environment variable is invalid: ", err)
	} else {
		fmt.Println("LOG_LEVEL environment variable is not set, using default level: DISABLED")
	}
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnixMs
	log.Logger = zerolog.New(zerolog.NewConsoleWriter(func(w *zerolog.ConsoleWriter) {
		w.TimeFormat = "04:05.000ms"
	})).With().Timestamp().Logger()
}

func SetupMocksForEvmStatePoller() {
	ResetGock()

	// Mock for evm block tracker
	gock.New("http://rpc1.localhost").
		Post("").
		Persist().
		Filter(func(request *http.Request) bool {
			return strings.Contains(SafeReadBody(request), "eth_getBlockByNumber")
		}).
		Reply(200).
		JSON([]byte(`{"result": {"number":"0x1273c18"}}`))
	gock.New("http://rpc1.localhost").
		Post("").
		Persist().
		Filter(func(request *http.Request) bool {
			return strings.Contains(SafeReadBody(request), "eth_syncing")
		}).
		Reply(200).
		JSON([]byte(`{"result":false}`))
	gock.New("http://rpc2.localhost").
		Post("").
		Persist().
		Filter(func(request *http.Request) bool {
			body := SafeReadBody(request)
			return strings.Contains(body, "eth_getBlockByNumber") && (strings.Contains(body, "latest") || strings.Contains(body, "finalized"))
		}).
		Reply(200).
		JSON([]byte(`{"result": {"number":"0x1273c18"}}`))
	gock.New("http://rpc2.localhost").
		Post("").
		Persist().
		Filter(func(request *http.Request) bool {
			return strings.Contains(SafeReadBody(request), "eth_syncing")
		}).
		Reply(200).
		JSON([]byte(`{"result":false}`))
}

func AnyTestMocksLeft() int {
	// We have 4 persisted mocks for evm block tracker
	return len(gock.Pending()) - 4
}

func ResetGock() {
	gock.OffAll()
	gock.Clean()
	gock.CleanUnmatchedRequest()
	gock.Disable()
}

func SafeReadBody(request *http.Request) string {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return ""
	}
	request.Body = io.NopCloser(bytes.NewBuffer(body))
	return string(body)
}
