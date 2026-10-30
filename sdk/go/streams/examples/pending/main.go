// Pending prints pending transaction hashes and exits after BATCHES
// batches (default: never).
//
//	ERPC_ENDPOINT=grpc://localhost:4001/main/evm/1 go run ./examples/pending
package main

import (
	"context"
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
	limit, _ := strconv.Atoi(os.Getenv("BATCHES"))
	batches := 0
	err := streams.Run(ctx, streams.Config{
		Endpoints: []string{os.Getenv("ERPC_ENDPOINT")},
		Kind:      streams.NewPendingTransactions,
	}, streams.Handler{
		Apply: func(ctx context.Context, b streams.Block) error {
			for _, h := range b.Hashes {
				fmt.Println(h)
			}
			if batches++; batches == limit {
				stop()
			}
			return nil
		},
	})
	if err != nil {
		log.Fatal(err)
	}
}
