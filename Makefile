.PHONY: help
help:
	@echo
	@echo "Usage: make [command]"
	@echo
	@echo "Commands:"
	@echo " build                         Build the eRPC server"
	@echo
	@echo " docker-up                     Up docker services"
	@echo " docker-down                   Down docker services"
	@echo
	@echo " fmt                           Format source code"
	@echo " test                          Run unit tests"
	@echo

.PHONY: setup
setup:
	@go mod tidy

.PHONY: run
run:
	@go run ./cmd/erpc/main.go

.PHONY: build
build:
	@CGO_ENABLED=0 go build -ldflags="-w -s" -o ./bin/erpc ./cmd/erpc/main.go
	@echo executable file \"erpc\" saved in ./bin/erpc

.PHONY: test
test:
	@go test ./... -v

.PHONY: coverage
coverage:
	@go test -coverprofile=coverage.out ./... -v
	@go tool cover -html=coverage.out

.PHONY: docker-up
docker-up:
	@docker-compose up -d

.PHONY: docker-down
docker-down:
	@docker-compose down

.PHONY: fmt
fmt:
	@go fmt ./...

.DEFAULT_GOAL := help