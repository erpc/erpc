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
	@echo " docker-build-validator        Build validator docker image. Arg: platform=linux/amd64 (default) or platform=linux/arm64"
	@echo " docker-build-both             Build both images locally. Args: platform=linux/amd64 repo=local tag=local"
	@echo " docker-push-both              Build+push both images (multi-arch). Args: repo=morphoorg tag=... platforms=linux/amd64,linux/arm64"
	@echo " docker-run                    Run docker image. Arg: platform=linux/amd64 (default) or platform=linux/arm64"
	@echo " docker-run-validator          Validate config in validator image. Arg: platform=linux/amd64 (default) or platform=linux/arm64"

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
	@go test $$(ls -d */ | grep -v "cmd/" | grep -v "test/" | awk '{print "./" $$1 "..."}') -covermode=atomic -v -race -count 1 -parallel 1 -timeout 15m -failfast=false

.PHONY: test-fast
test-fast:
	@go clean -testcache
	@go test ./cmd/... -count 1 -parallel 1 -v
	@go test $$(ls -d */ | grep -v "cmd/" | grep -v "test/" | awk '{print "./" $$1 "..."}') -count 1 -parallel 1 -v -timeout 10m -failfast=false

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
