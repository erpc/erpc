# erpc — Claude Code Guide

See `.cursorrules` for the full project guide (dev setup, testing, error handling, etc). This file covers patterns that are easy to get wrong.

## Go Concurrency Patterns

### Grab Under Lock, Use Outside
When reading shared fields that another goroutine may write, copy them to locals under `RLock`, then use the locals after releasing:
```go
e.stateMu.RLock()
cfg := e.cfg
label := e.networkLabel
e.stateMu.RUnlock()

// use cfg and label freely here
```
Never hold the lock while doing slow work (RPC calls, I/O, channel sends).

### Atomics for Hot-Path Counters
Use `atomic.Int64` / `atomic.Uint64` for single values read and written from multiple goroutines. Prefer atomics over a mutex when you only need a single load/store — they're cheaper and can't deadlock.

### Config Setters and Consumers Run on Different Goroutines
Methods like `SetNetworkConfig` are called from the project registry goroutine, while the ticker goroutine and request-serving goroutines read that config. Any field shared between them must be protected by a mutex or atomic. When adding new shared state, check who writes it and who reads it.
