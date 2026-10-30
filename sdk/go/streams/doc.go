// Package streams streams blocks, logs and pending transactions from eRPC
// over gRPC into an application, with resume from a stored cursor, reorgs
// as Undo calls, heartbeat watchdog, reconnects and an optional second
// endpoint merged into one chain.
//
//	err := streams.Run(ctx, streams.Config{
//		Endpoints: []string{"grpcs://erpc-a.example:443/main/evm/1"},
//		Kind:      streams.Logs,
//		Filter:    streams.Filter{Address: []string{"0xa0b8…"}},
//		Finality:  streams.Latest,
//		Cursor:    streams.FileCursor("erpc.cursor"),
//	}, streams.Handler{Apply: apply, Undo: undo})
//
// Apply and Undo run on one goroutine, in chain order. The cursor is saved
// after each returns nil: delivery is at least once, so both must be
// idempotent; key logs by Log.Key.
package streams
