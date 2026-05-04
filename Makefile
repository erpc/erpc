.PHONY: help
help:
	@echo
	@echo "Usage: make [command]"
	@echo
	@echo "Commands:"
	@echo " build                         Build the eRPC server"
	@echo " fmt                           Format source code"
	@echo " test                          Run unit tests"
	@echo
	@echo " run-k6                        Run k6 tests"
	@echo " run-pprof                     Run the eRPC server with pprof"
	@echo " run-fake-rpcs                 Run fake RPCs"
	@echo " up                            Up docker services"
	@echo " down                          Down docker services"
	@echo " fmt                           Format source code"
	@echo " docker-build                  Build docker image. Arg: platform=linux/amd64 (default) or platform=linux/arm64"
	@echo " docker-run                    Run docker image. Arg: platform=linux/amd64 (default) or platform=linux/arm64"

.PHONY: setup
setup:
	@go mod tidy

.PHONY: run
run:
	@go run ./cmd/erpc/main.go

.PHONY: run-pprof
run-pprof:
	@go run ./cmd/erpc/main.go ./cmd/erpc/pprof.go

.PHONY: run-fake-rpcs
run-fake-rpcs:
	@go run ./test/cmd/main.go

.PHONY: run-k6-evm-tip-of-chain
run-k6-evm-tip-of-chain:
	@k6 run --insecure-skip-tls-verify ./test/k6/evm-tip-of-chain.js

.PHONY: run-k6-evm-historical-randomized
run-k6-evm-historical-randomized:
	@k6 run --insecure-skip-tls-verify --summary-export=summary.json ./test/k6/evm-historical-randomized.js

.PHONY: build
build:
	@CGO_ENABLED=0 go build -ldflags="-w -s" -o ./bin/erpc-server ./cmd/erpc/main.go
	@CGO_ENABLED=0 go build -ldflags="-w -s" -tags pprof -o ./bin/erpc-server-pprof ./cmd/erpc/*.go

.PHONY: test
test:
	@go clean -testcache
	@go test ./cmd/... -count 1 -parallel 1
	@go test $$(ls -d */ | grep -v "cmd/" | grep -v "test/" | awk '{print "./" $$1 "..."}') -covermode=atomic -v -race -count 1 -parallel 1 -timeout 30m -failfast=false

.PHONY: test-fast
test-fast:
	@go clean -testcache
	@go test ./cmd/... -count 1 -parallel 1 -v
	@# Compile erpc test binary once
	@go test -c -o /tmp/erpc.test ./erpc
	@# Run erpc shards in parallel (each process has isolated gock state)
	@# DSL shard (requires OpenAI API key, non-blocking)
	@/tmp/erpc.test -test.v -test.timeout 5m -test.run "^TestConsensusPolicy_DSL" || true &
	@# Named shards run in parallel (each process has isolated gock state)
	@# Shard definitions are only for the known-slow test groups.
	@# The catch-all shard automatically picks up any new/unassigned tests.
	@bash -c '\
	  SHARD_CONSENSUS="^(TestConsensusPolicy$$|TestConsensusGoroutine|TestConsensusSelectsLargest|TestInterpolation_)"; \
	  SHARD_NETWORK="^(TestNetwork_Forward$$|TestNetwork_(SelectionScenarios|InFlightRequests|SkippingUpstreams|EvmGetLogs|ThunderingHerd|HighestFinalizedBlockNumber|HighestLatestBlockNumber|CacheEmptyBehavior|BatchRequests|SingleRequestErrors|CapacityExceededErrors|TraceExecutionTimeout|Multiplexer|SendRawTransaction))"; \
	  SHARD_HTTPSERVER="^TestHttpServer_"; \
	  SHARD_INTEGRITY="^(TestNetworkRetry_|TestNetworkForward_|TestNetworkIntegrity_|TestHedgeConsensus_|TestEmptyResultAcceptShortCircuit$$|TestNetworkFailsafe_|TestNetworksBootstrap_|TestNetworkAvailability_|TestNetwork_Forward_InfiniteLoopWithAllUpstreamsSkipping$$|TestNetwork_HedgePolicy)"; \
	  SKIP_ALL="TestConsensusPolicy|TestConsensusGoroutine|TestConsensusSelectsLargest|TestInterpolation_|TestNetwork_|TestHttpServer_|TestHttp_|TestNetworkRetry_|TestNetworkForward_|TestNetworkIntegrity_|TestHedgeConsensus_|TestEmptyResultAcceptShortCircuit|TestNetworkFailsafe_|TestNetworksBootstrap_|TestNetworkAvailability_"; \
	  pids=(); \
	  /tmp/erpc.test -test.v -test.timeout 10m -test.run "$$SHARD_CONSENSUS" -test.parallel 4 & pids+=($$!); \
	  /tmp/erpc.test -test.v -test.timeout 10m -test.run "$$SHARD_NETWORK" & pids+=($$!); \
	  /tmp/erpc.test -test.v -test.timeout 10m -test.run "$$SHARD_HTTPSERVER" & pids+=($$!); \
	  /tmp/erpc.test -test.v -test.timeout 10m -test.run "$$SHARD_INTEGRITY" & pids+=($$!); \
	  /tmp/erpc.test -test.v -test.timeout 10m -test.skip "$$SKIP_ALL" & pids+=($$!); \
	  fail=0; for pid in "$${pids[@]}"; do wait "$$pid" || fail=1; done; \
	  exit $$fail'
	@# TestHttp_ tests run after parallel shards (sensitive to system load, only ~5s)
	@/tmp/erpc.test -test.v -test.timeout 5m -test.run "^TestHttp_"
	@# Run all other (non-erpc) packages normally
	@go test $$(ls -d */ | grep -v "cmd/" | grep -v "test/" | grep -v "erpc/" | awk '{print "./" $$1 "..."}') -count 1 -parallel 4 -v -timeout 30m -failfast=false

.PHONY: test-race
test-race:
	@go clean -testcache
	@go test ./cmd/... -count 5 -parallel 5 -v -race
	@go test $$(ls -d */ | grep -v "cmd/" | grep -v "test/" | awk '{print "./" $$1 "..."}') -count 15 -parallel 15 -v -timeout 30m -race

.PHONY: bench
bench:
	@go test -run=^$$ -bench=. -benchmem -count=8 -v ./...

.PHONY: coverage
coverage:
	@go clean -testcache
	@go test -coverprofile=coverage.txt -covermode=atomic $$(ls -d */ | grep -v "cmd/" | grep -v "test/" | awk '{print "./" $$1 "..."}')
	@go tool cover -html=coverage.txt

.PHONY: up
up:
	@docker compose up -d --force-recreate --build --remove-orphans

.PHONY: down
down:
	@docker compose down --volumes

.PHONY: fmt
fmt:
	@go fmt ./...

.PHONY: docker-build
docker-build:
	$(eval platform ?= linux/amd64)
	@docker build -t local/erpc-server --platform $(platform) --build-arg VERSION=local --build-arg COMMIT_SHA=$$(git rev-parse --short HEAD) .

.PHONY: docker-run
docker-run:
	$(eval platform ?= linux/amd64)
	@docker run --rm -it --platform $(platform) -p 4000:4000 -p 4001:4001 -v $(PWD)/erpc.yaml:/erpc.yaml local/erpc-server /erpc-server --config /erpc.yaml

.DEFAULT_GOAL := help
