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

# Build the application without pprof
RUN CGO_ENABLED=0 GOOS=linux LDFLAGS="-w -s -X common.ErpcVersion=${VERSION} -X common.ErpcCommitSha=${COMMIT_SHA}" go build -a -installsuffix cgo -o erpc-server ./cmd/erpc/main.go

# Build the application with pprof
RUN CGO_ENABLED=0 GOOS=linux LDFLAGS="-w -s -X common.ErpcVersion=${VERSION} -X common.ErpcCommitSha=${COMMIT_SHA}" go build -a -installsuffix cgo -tags pprof -o erpc-server-pprof ./cmd/erpc/*.go

# Final stage
FROM alpine:3.18

# Set the working directory
WORKDIR /root/

# Copy the binaries from the builder stage
COPY --from=builder /app/erpc-server /usr/bin/erpc-server
COPY --from=builder /app/erpc-server-pprof /usr/bin/erpc-server-pprof

RUN chmod 777 /usr/bin/erpc-server*

# 8080 -> erpc
# 6060 -> pprof (optional)
EXPOSE 8080 6060

# Use an environment variable to determine which binary to run
ENV PPROF=false

# Run the appropriate binary based on the environment variable
CMD if [ "$PPROF" = "true" ]; then /usr/bin/erpc-server-pprof; else /usr/bin/erpc-server; fi