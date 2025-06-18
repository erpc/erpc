package util

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

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
	}
	// else {
	// 	// fmt.Println("LOG_LEVEL environment variable is not set, using default level: DISABLED")
	// 	// zerolog.SetGlobalLevel(zerolog.TraceLevel)
	// }
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnixMs
	log.Logger = zerolog.New(zerolog.NewConsoleWriter(func(w *zerolog.ConsoleWriter) {
		w.TimeFormat = "04:05.000ms"
	})).With().Timestamp().Logger()
}

const (
	EvmBlockTrackerMocks = 8
)

func SetupMocksForEvmStatePoller() {
	ResetGock()

	// Mock for evm block tracker
	gock.New("http://rpc1.localhost").
		Post("").
		Persist().
		Filter(func(request *http.Request) bool {
			body := SafeReadBody(request)
			return strings.Contains(body, "eth_chainId")
		}).
		Reply(200).
		JSON([]byte(`{"result":"0x7b","_note":"detectFeatures expected mock for eth_chainId"}`))
	gock.New("http://rpc1.localhost").
		Post("").
		Persist().
		Filter(func(request *http.Request) bool {
			body := SafeReadBody(request)
			return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, "latest")
		}).
		Reply(200).
		JSON([]byte(`{"result":{"number":"0x11118888"},"_note":"evm state poller expected mock for latest block"}`))
	gock.New("http://rpc1.localhost").
		Post("").
		Persist().
		Filter(func(request *http.Request) bool {
			body := SafeReadBody(request)
			return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, "finalized")
		}).
		Reply(200).
		JSON([]byte(`{"result":{"number":"0x11117777"},"_note":"evm state poller expected mock for finalized block"}`))
	gock.New("http://rpc1.localhost").
		Post("").
		Persist().
		Filter(func(request *http.Request) bool {
			return strings.Contains(SafeReadBody(request), "eth_syncing")
		}).
		Reply(200).
		JSON([]byte(`{"result":false,"_note":"evm state poller expected mock for eth_syncing"}`))
	gock.New("http://rpc2.localhost").
		Post("").
		Persist().
		Filter(func(request *http.Request) bool {
			return strings.Contains(SafeReadBody(request), "eth_chainId")
		}).
		Reply(200).
		JSON([]byte(`{"result":"0x7b","_note":"detectFeatures expected mock for eth_chainId"}`))
	gock.New("http://rpc2.localhost").
		Post("").
		Persist().
		Filter(func(request *http.Request) bool {
			body := SafeReadBody(request)
			return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, "latest")
		}).
		Reply(200).
		JSON([]byte(`{"result":{"number":"0x22228888"},"_note":"evm state poller expected mock for latest block"}`))
	gock.New("http://rpc2.localhost").
		Post("").
		Persist().
		Filter(func(request *http.Request) bool {
			body := SafeReadBody(request)
			return strings.Contains(body, "eth_getBlockByNumber") && strings.Contains(body, "finalized")
		}).
		Reply(200).
		JSON([]byte(`{"result":{"number":"0x22227777"},"_note":"evm state poller expected mock for finalized block"}`))
	gock.New("http://rpc2.localhost").
		Post("").
		Persist().
		Filter(func(request *http.Request) bool {
			return strings.Contains(SafeReadBody(request), "eth_syncing")
		}).
		Reply(200).
		JSON([]byte(`{"result":false,"_note":"evm state poller expected mock for eth_syncing"}`))
}

func ResetGock() {
	gock.OffAll()
	gock.Clean()
	gock.CleanUnmatchedRequest()
	gock.Disable()

	gock.EnableNetworking()
	gock.NetworkingFilter(func(req *http.Request) bool {
		shouldMakeRealCall := strings.Split(req.URL.Host, ":")[0] == "localhost"
		return shouldMakeRealCall
	})

	time.Sleep(100 * time.Millisecond)
}

func SafeReadBody(request *http.Request) string {
	if request.Body == nil {
		return ""
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return ""
	}
	request.Body = io.NopCloser(bytes.NewBuffer(body))
	return string(body)
}

func AssertNoPendingMocks(t *testing.T, expected int) {
	totalPending := len(gock.Pending())
	totalExpected := expected + EvmBlockTrackerMocks
	if totalPending != totalExpected {
		totalPendingUserMocks := 0
		for _, pending := range gock.Pending() {
			buff := pending.Response().BodyBuffer
			if strings.Contains(string(buff), "expected mock") {
				continue
			}
			totalPendingUserMocks++
			if len(buff) > 1024 {
				t.Errorf("Pending mock: %v -> %v", pending.Request().URLStruct.String(), string(pending.Response().BodyBuffer[:1024]))
			} else {
				t.Errorf("Pending mock: %v -> %v", pending.Request().URLStruct.String(), string(pending.Response().BodyBuffer))
			}
		}
		totalExpectedUserMocks := totalExpected - EvmBlockTrackerMocks
		t.Errorf("Expected %v mocks to be pending, got %v left", totalExpectedUserMocks, totalPendingUserMocks)
	}
}
