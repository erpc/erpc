// Logs streams a contract's logs into a map keyed by Log.Key, resuming
// from erpc.cursor, and exits after BLOCKS blocks (default: never).
//
//	ERPC_ENDPOINT=grpc://localhost:4001/main/evm/1 ERPC_ADDRESS=0x… go run ./examples/logs
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"

	"github.com/erpc/erpc/sdk/go/streams"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	limit, _ := strconv.Atoi(os.Getenv("BLOCKS"))

	held := map[string]uint64{} // log key → block number
	blocks := 0
	var filter streams.Filter
	if a := os.Getenv("ERPC_ADDRESS"); a != "" {
		filter.Address = []string{a}
	}
	md := map[string]string{}
	if t := os.Getenv("ERPC_TOKEN"); t != "" {
		md["x-erpc-secret-token"] = t
	}
	err := streams.Run(ctx, streams.Config{
		Endpoints: []string{os.Getenv("ERPC_ENDPOINT")},
		Kind:      streams.Logs,
		Filter:    filter,
		Finality:  streams.Latest,
		Cursor:    streams.FileCursor("erpc.cursor"),
		Metadata:  md,
	}, streams.Handler{
		Apply: func(ctx context.Context, b streams.Block) error {
			for _, l := range b.Logs {
				held[l.Key()] = b.Number
			}
			fmt.Printf("block %d %s: %d logs\n", b.Number, b.Hash, len(b.Logs))
			if blocks++; blocks == limit {
				stop()
			}
			return nil
		},
		Undo: func(ctx context.Context, fp streams.BlockRef) error {
			for k, n := range held {
				if n > fp.Number {
					delete(held, k)
				}
			}
			fmt.Printf("undo to %d %s\n", fp.Number, fp.Hash)
			return nil
		},
		Status: func(s streams.Status) { fmt.Printf("status %s %s %s\n", s.Endpoint, s.State, s.Reason) },
	})
	var oor *streams.OutOfRangeError
	if errors.As(err, &oor) {
		log.Fatalf("cursor %s is older than the server's window (oldest %v): backfill with eth_getLogs, then delete erpc.cursor", oor.Cursor, oor.Oldest)
	}
	if err != nil {
		log.Fatal(err)
	}
}
