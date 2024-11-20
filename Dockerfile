# syntax = docker/dockerfile:1.4

# Build stage
FROM golang:1.22-alpine AS builder

# Set the working directory
WORKDIR /root

# Copy go mod and sum files first for better layer caching
COPY go.mod go.sum ./

# Download dependencies in a separate layer
RUN go mod download

# Copy the source code
COPY . .

# Set build arguments
ARG VERSION
ARG COMMIT_SHA

# Set environment variables
ENV CGO_ENABLED=0 \
    GOOS=linux \
    LDFLAGS="-w -s -X common.ErpcVersion=${VERSION} -X common.ErpcCommitSha=${COMMIT_SHA}"

# Build the binary
RUN go build -ldflags="$LDFLAGS" -a -installsuffix cgo -o erpc-server ./cmd/erpc/main.go

# Final stage - using distroless for smaller image
FROM debian:12 AS final

WORKDIR /root

# Copy binary from the builder
COPY --from=builder /root/erpc-server .

# Expose ports
EXPOSE 8080 6060

# Run the appropriate binary
CMD ["/root/erpc-server"]
