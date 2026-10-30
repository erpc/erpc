// Merge follows new heads from two eRPC endpoints merged into one chain,
// finalized only, and exits after BLOCKS blocks (default: never).
//
//	ERPC_ENDPOINTS=grpc://a:4001/main/evm/1,grpc://b:4001/main/evm/1 go run ./examples/merge
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"

	"github.com/erpc/erpc/sdk/go/streams"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	limit, _ := strconv.Atoi(os.Getenv("BLOCKS"))
	blocks := 0
	err := streams.Run(ctx, streams.Config{
		Endpoints: strings.Split(os.Getenv("ERPC_ENDPOINTS"), ","),
		Kind:      streams.NewHeads,
		Finality:  streams.Finalized,
		Cursor:    streams.FileCursor("heads.cursor"),
	}, streams.Handler{
		Apply: func(ctx context.Context, b streams.Block) error {
			fmt.Printf("final block %d %s at %d\n", b.Number, b.Hash, b.Header.Timestamp)
			if blocks++; blocks == limit {
				stop()
			}
			return nil
		},
		Status: func(s streams.Status) { fmt.Printf("status %s %s %s\n", s.Endpoint, s.State, s.Reason) },
	})
	if err != nil {
		log.Fatal(err)
	}
}
