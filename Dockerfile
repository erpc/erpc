# syntax = docker/dockerfile:1.4

# Build stage for Go
FROM golang:1.22-alpine AS go-builder

WORKDIR /root

# Copy go mod and sum files first for better layer caching
COPY go.mod go.sum ./
RUN go mod download

# Copy the source code
COPY . .

# Set build arguments
ARG VERSION
ARG COMMIT_SHA

# Set environment variables for Go build
ENV CGO_ENABLED=0 \
    GOOS=linux \
    LDFLAGS="-w -s -X common.ErpcVersion=${VERSION} -X common.ErpcCommitSha=${COMMIT_SHA}"

# Build the Go binary
RUN go build -ldflags="$LDFLAGS" -a -installsuffix cgo -o erpc-server ./cmd/erpc/main.go

# Build stage for TypeScript package
FROM node:20-alpine AS ts-builder

WORKDIR /root

# Install pnpm
RUN npm install -g pnpm

# Copy only the TypeScript package files
COPY typescript /root/typescript
COPY package.json /root/package.json
COPY pnpm* /root/

# Install dependencies and build
RUN pnpm install && pnpm build

# Final stage
FROM debian:12 AS final

WORKDIR /root

# Install CA certificates
RUN apt-get update && apt-get install -y ca-certificates && rm -rf /var/lib/apt/lists/*

# Copy Go binary from go-builder
COPY --from=go-builder /root/erpc-server .

# Copy TypeScript package files from ts-builder
COPY --from=ts-builder /root/node_modules ./node_modules
COPY --from=ts-builder /root/typescript ./typescript

# Expose ports
EXPOSE 8080 6060

# Run the server
CMD ["/root/erpc-server"]