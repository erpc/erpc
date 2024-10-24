# Build stage
FROM golang:1.22-alpine AS builder

ARG VERSION
ARG COMMIT_SHA

# Set the working directory
WORKDIR /app

# Copy go mod and sum files
COPY go.mod go.sum ./

# Download all dependencies
RUN go mod download

# Copy the source code
COPY . .

# Set environment variables
ENV CGO_ENABLED=0 \
    GOOS=linux \
    LDFLAGS="-w -s -X common.ErpcVersion=${VERSION} -X common.ErpcCommitSha=${COMMIT_SHA}"

# Build both binaries in parallel
RUN sh -c ' \
    go build -a -installsuffix cgo -ldflags "$LDFLAGS" -o erpc-server ./cmd/erpc/main.go & \
    go build -a -installsuffix cgo -tags pprof -ldflags "$LDFLAGS" -o erpc-server-pprof ./cmd/erpc/*.go & \
    wait \
'

# Final stage
FROM alpine:3.18

# Set the working directory
WORKDIR /root/

# Copy the binaries from the builder stage
COPY --from=builder /app/erpc-server .
COPY --from=builder /app/erpc-server-pprof .

# Expose ports
EXPOSE 8080 6060

# Use an environment variable to determine which binary to run
ENV PPROF=false

# Run the appropriate binary based on the environment variable
CMD if [ "$PPROF" = "true" ]; then ./erpc-server-pprof; else ./erpc-server; fi
