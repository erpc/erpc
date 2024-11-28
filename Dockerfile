# syntax = docker/dockerfile:1.4

# This Dockerfile is used to build the eRPC server image.
# Docker build stages:
#     - go-builder -> build the go binary
#     - ts-core -> Core stage for TS related stuff (just installing pnpm)
#     - ts-dev -> Install dev dependencies and compile the SDK
#     - ts-prod -> Install prod dependencies only
#     - final -> Final stage where we copy the Go binary and the TS files

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

# Global typescript related image
FROM node:20-alpine AS ts-core
RUN npm install -g pnpm

# Stage where we will install dev dependencies + compile sdk
FROM ts-core AS ts-dev
RUN mkdir -p /temp/dev

# Copy only the TypeScript package files
COPY typescript /temp/dev/typescript
COPY pnpm* /temp/dev/
COPY package.json /temp/dev/package.json

# Install everything and build
RUN --mount=type=cache,id=pnpm,target=/pnpm/store cd /temp/dev &&  pnpm install --frozen-lockfile 
RUN cd /temp/dev && pnpm build

# Stage where we will install prod dependencies only
FROM ts-core AS ts-prod
RUN mkdir -p /temp/prod

COPY typescript /temp/prod/typescript
COPY pnpm* /temp/prod/
COPY package.json /temp/prod/package.json

# Install every prod dependencies
RUN --mount=type=cache,id=pnpm,target=/pnpm/store cd /temp/prod && pnpm install --prod --frozen-lockfile

# Final stage
FROM debian:12 AS final

WORKDIR /root

# Install CA certificates
RUN apt-get update && apt-get install -y ca-certificates && rm -rf /var/lib/apt/lists/*

# Copy Go binary from go-builder
COPY --from=go-builder /root/erpc-server .

# Copy TypeScript package files from ts-builder
COPY --from=ts-dev /temp/dev/typescript ./typescript

# Copy node_modules from ts-prod
COPY --from=ts-prod /temp/prod/node_modules ./node_modules

# Expose ports
EXPOSE 8080 6060

# Run the server
CMD ["/root/erpc-server"]