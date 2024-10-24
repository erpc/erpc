# syntax = docker/dockerfile:1.4

# Build stage
FROM golang:1.22-alpine AS builder

# Enable build cache
ENV GOCACHE=/go-cache
ENV GOMODCACHE=/go-mod-cache

# Set the working directory
WORKDIR /root

# Copy go mod and sum files first for better layer caching
COPY go.mod go.sum ./

# Download dependencies in a separate layer
RUN --mount=type=cache,target=/go-mod-cache \
    go mod download

# Copy the source code
COPY . .

# Set build arguments
ARG VERSION
ARG COMMIT_SHA

# Set environment variables
ENV CGO_ENABLED=0 \
    GOOS=linux \
    LDFLAGS="-w -s -X common.ErpcVersion=${VERSION} -X common.ErpcCommitSha=${COMMIT_SHA}"

# Build both binaries in parallel
RUN --mount=type=cache,target=/go-cache \
    --mount=type=cache,target=/go-mod-cache \
    sh -c ' \
        go build -ldflags="$LDFLAGS" -a -installsuffix cgo -o erpc-server ./cmd/erpc/main.go & \
        go build -ldflags="$LDFLAGS" -a -installsuffix cgo -tags pprof -o erpc-server-pprof ./cmd/erpc/*.go & \
        wait \
    '

# Final stage - using distroless for smaller image
FROM gcr.io/distroless/static-debian11 AS final

WORKDIR /root

# Copy binaries from the builder
COPY --from=builder /root/erpc-server .
COPY --from=builder /root/erpc-server-pprof .

# Expose ports
EXPOSE 8080 6060

# Run the appropriate binary
CMD ["/root/erpc-server"]
