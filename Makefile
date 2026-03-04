.PHONY: help
help:
	@echo
	@echo "Usage: make [command]"
	@echo
	@echo "Commands:"
	@echo " build                         Build the eRPC server"
	@echo " fmt                           Format source code"
	@echo " test                          Run unit tests"
	@echo " test-fallback-config          Run fallback-path regression suite"
	@echo " test-parallel                 Run unit tests with higher parallelism (defaults: TEST_PARALLEL=8 TEST_P=8)"
	@echo " test-parallel-max             Run unit tests with aggressive parallelism (defaults: TEST_PARALLEL=16 TEST_P=16)"
	@echo " agent-context                 Print repo + impact context for autonomous loops"
	@echo " agent-refresh                 Regenerate review/repo-map.md"
	@echo " agent-check                   Verify autonomous harness artifacts"
	@echo " agent-gate                    Harness check + build + fast tests"
	@echo " agent-gate-full               Harness check + build + full tests"
	@echo " agent-skills-shell-check      Verify skills-routing and shell guidance guardrails"
	@echo " agent-review-load             Estimate review bottleneck risk for current diff"
	@echo " agent-pr-health               Show PR mergeability/check-health (requires gh auth)"
	@echo " agent-random-bug              Run random latent-bug package scan"
	@echo " agent-digest                  Generate contribution digest (last 24h)"
	@echo
	@echo " run-k6                        Run k6 tests"
	@echo " run-pprof                     Run the eRPC server with pprof"
	@echo " run-fake-rpcs                 Run fake RPCs"
	@echo " up                            Up docker services"
	@echo " down                          Down docker services"
	@echo " fmt                           Format source code"
	@echo " docker-build                  Build docker image. Arg: platform=linux/amd64 (default) or platform=linux/arm64"
	@echo " docker-build-validator        Build validator docker image. Arg: platform=linux/amd64 (default) or platform=linux/arm64"
	@echo " docker-build-both             Build both images locally. Args: platform=linux/amd64 repo=local tag=local"
	@echo " docker-push-both              Build+push both images (multi-arch). Args: repo=morphoorg tag=... platforms=linux/amd64,linux/arm64"
	@echo " docker-run                    Run docker image. Arg: platform=linux/amd64 (default) or platform=linux/arm64"
	@echo " docker-run-validator          Validate config in validator image. Arg: platform=linux/amd64 (default) or platform=linux/arm64"

.PHONY: setup
setup:
	@go mod tidy

.PHONY: agent-context
agent-context:
	@scripts/agent-harness/context.sh

.PHONY: agent-refresh
agent-refresh:
	@scripts/agent-harness/update-repo-map.sh

.PHONY: agent-check
agent-check:
	@scripts/agent-harness/check.sh

.PHONY: agent-gate
agent-gate:
	@scripts/agent-harness/gate.sh

.PHONY: agent-gate-full
agent-gate-full:
	@scripts/agent-harness/gate.sh --full

.PHONY: agent-skills-shell-check
agent-skills-shell-check:
	@scripts/agent-harness/skills-shell-check.sh

.PHONY: agent-review-load
agent-review-load:
	@scripts/agent-harness/review-load.sh

.PHONY: agent-pr-health
agent-pr-health:
	@scripts/agent-harness/pr-health.sh

.PHONY: agent-random-bug
agent-random-bug:
	@scripts/agent-harness/random-bug-scan.sh --count 1

.PHONY: agent-digest
agent-digest:
	@scripts/agent-harness/merged-digest.sh --since "24 hours ago"

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

TEST_PARALLEL ?= 1
TEST_P ?= $(TEST_PARALLEL)
PKG_EXCLUDE_REGEX := ^github\.com/erpc/erpc/(cmd|test)($$|/)
HAS_RG := $(shell command -v rg >/dev/null 2>&1 && echo 1 || echo 0)
ifeq ($(HAS_RG),1)
PKG_FILTER_CMD := rg -v '$(PKG_EXCLUDE_REGEX)'
else
PKG_FILTER_CMD := grep -Ev '$(PKG_EXCLUDE_REGEX)'
endif
GO_TEST_PACKAGES := $$(go list ./... | $(PKG_FILTER_CMD))

.PHONY: test
test:
	@go clean -testcache
	@go test ./cmd/... -count 1 -parallel $(TEST_PARALLEL) -p $(TEST_P)
	@go test $(GO_TEST_PACKAGES) -covermode=atomic -v -race -count 1 -parallel $(TEST_PARALLEL) -p $(TEST_P) -timeout 15m -failfast=false

.PHONY: test-fallback-config
test-fallback-config:
	@go clean -testcache
	@go test ./erpc -list '^TestPolicyEvaluator$$' | grep -q '^TestPolicyEvaluator$$'
	@go test ./erpc -run 'TestPolicyEvaluator/DefaultPolicy' -count=1
	@go test ./erpc -list '^TestNetworkForward_TryAllUpstreams_FallbackWithinSameRound$$' | grep -q '^TestNetworkForward_TryAllUpstreams_FallbackWithinSameRound$$'
	@go test ./erpc -run '^TestNetworkForward_TryAllUpstreams_FallbackWithinSameRound$$' -count=1
	@go test ./erpc -list '^TestHandleEthCallBatchAggregation_FallbackPaths$$' | grep -q '^TestHandleEthCallBatchAggregation_FallbackPaths$$'
	@go test ./erpc -run '^TestHandleEthCallBatchAggregation_FallbackPaths$$' -count=1
	@go test ./architecture/evm -list '^TestShouldFallbackMulticall3$$' | grep -q '^TestShouldFallbackMulticall3$$'
	@go test ./architecture/evm -list '^TestBatcherFlushFallbackOnMulticall3Unavailable$$' | grep -q '^TestBatcherFlushFallbackOnMulticall3Unavailable$$'
	@go test ./architecture/evm -list '^TestBatcher_FallbackIndividual_PanicRecovery$$' | grep -q '^TestBatcher_FallbackIndividual_PanicRecovery$$'
	@go test ./architecture/evm -run 'TestShouldFallbackMulticall3|TestBatcherFlushFallbackOnMulticall3Unavailable|TestBatcher_FallbackIndividual_PanicRecovery' -count=1

.PHONY: test-fast
test-fast:
	@go clean -testcache
	@go test ./cmd/... -count 1 -parallel $(TEST_PARALLEL) -p $(TEST_P) -v
	@go test $(GO_TEST_PACKAGES) -count 1 -parallel $(TEST_PARALLEL) -p $(TEST_P) -v -timeout 10m -failfast=false

.PHONY: test-parallel
test-parallel:
	@$(MAKE) test TEST_PARALLEL=$${TEST_PARALLEL:-12} TEST_P=$${TEST_P:-12}

.PHONY: test-parallel-max
test-parallel-max:
	@$(MAKE) test TEST_PARALLEL=$${TEST_PARALLEL:-16} TEST_P=$${TEST_P:-16}

.PHONY: test-race
test-race:
	@go clean -testcache
	@go test ./cmd/... -count 5 -parallel 5 -v -race
	@go test $(GO_TEST_PACKAGES) -count 15 -parallel 15 -p 15 -v -timeout 30m -race

.PHONY: bench
bench:
	@go test -run=^$$ -bench=. -benchmem -count=8 -v ./...

.PHONY: coverage
coverage:
	@go clean -testcache
	@go test -coverprofile=coverage.txt -covermode=atomic $(GO_TEST_PACKAGES)
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

.PHONY: docker-build-validator
docker-build-validator:
	$(eval platform ?= linux/amd64)
	@docker build -f Dockerfile.validator -t local/erpc-validator --platform $(platform) --build-arg VERSION=local --build-arg COMMIT_SHA=$$(git rev-parse --short HEAD) .

.PHONY: docker-build-both
docker-build-both:
	$(eval platform ?= linux/amd64)
	$(eval repo ?= local)
	$(eval tag ?= local)
	@docker build -t $(repo)/erpc:$(tag) --platform $(platform) --build-arg VERSION=$(tag) --build-arg COMMIT_SHA=$$(git rev-parse --short HEAD) .
	@docker build -f Dockerfile.validator -t $(repo)/erpc-validator:$(tag) --platform $(platform) --build-arg VERSION=$(tag) --build-arg COMMIT_SHA=$$(git rev-parse --short HEAD) .

.PHONY: docker-push-both
docker-push-both:
	$(eval repo ?= ghcr.io/erpc)
	$(eval tag ?= 0.0.0-dev-$$(date -u +%Y%m%d%H%M)-g$$(git rev-parse --short HEAD))
	$(eval platforms ?= linux/amd64,linux/arm64)
	@REPO=$(repo) TAG=$(tag) PLATFORMS=$(platforms) PUSH=true ./scripts/docker/build-push-images.sh

.PHONY: docker-run
docker-run:
	$(eval platform ?= linux/amd64)
	@docker run --rm -it --platform $(platform) -p 4000:4000 -p 4001:4001 -v $(PWD)/erpc.yaml:/erpc.yaml local/erpc-server /erpc-server --config /erpc.yaml

.PHONY: docker-run-validator
docker-run-validator:
	$(eval platform ?= linux/amd64)
	@docker run --rm --platform $(platform) -v $(PWD)/erpc.yaml:/erpc.yaml local/erpc-validator /usr/local/bin/erpc validate --config /erpc.yaml

.DEFAULT_GOAL := help
