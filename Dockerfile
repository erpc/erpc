# syntax = docker/dockerfile:1.4

# This Dockerfile is used to build the eRPC server image.
# Docker build stages:
#     - go-builder -> build the go binary
#     - ts-core -> Core stage for TS related stuff (just installing pnpm)
#     - ts-dev -> Install dev dependencies and compile the SDK
#     - ts-prod -> Install prod dependencies only
#     - final -> Final stage where we copy the Go binary and the TS files

# Build stage for Go
FROM golang:1.23-alpine AS go-builder

WORKDIR /build

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
    GOARCH=amd64 \
    LDFLAGS="-w -s -X common.ErpcVersion=${VERSION} -X common.ErpcCommitSha=${COMMIT_SHA}"

# Build the Go binary
RUN go build -v -ldflags="$LDFLAGS" -a -installsuffix cgo -o erpc-server ./cmd/erpc/main.go && \
    go build -v -ldflags="$LDFLAGS" -a -installsuffix cgo -tags pprof -o erpc-server-pprof ./cmd/erpc/*.go

# Global typescript related image
FROM node:20-alpine AS ts-core
RUN npm install -g pnpm

# Stage where we will install dev dependencies + compile sdk
FROM ts-core AS ts-dev
RUN mkdir -p /temp/dev/typescript
RUN npm install -g pnpm

# Copy only the TypeScript package files
COPY typescript/config /temp/dev/typescript/config
COPY pnpm* /temp/dev/
COPY package.json /temp/dev/package.json

# Install everything and build
RUN --mount=type=cache,id=pnpm,target=/pnpm/store cd /temp/dev &&  pnpm install --frozen-lockfile 
RUN cd /temp/dev && pnpm build

# Stage where we will install prod dependencies only
FROM ts-core AS ts-prod
RUN mkdir -p /temp/prod/typescript

COPY typescript/config /temp/prod/typescript/config
COPY pnpm* /temp/prod/
COPY package.json /temp/prod/package.json

# Install every prod dependencies
RUN --mount=type=cache,id=pnpm,target=/pnpm/store cd /temp/prod && pnpm install --prod --frozen-lockfile

# Create symlink stage
FROM alpine:latest AS symlink
RUN mkdir -p /root && ln -s /erpc-server /root/erpc-server

# Final stage
FROM gcr.io/distroless/static-debian12:nonroot AS final

# Copy Go binary from go-builder
COPY --from=go-builder /build/erpc-server /
COPY --from=go-builder /build/erpc-server-pprof /

# Copy symlinked directory
COPY --from=symlink /root /root

# Copy TypeScript package files from ts-dev and ts-prod
COPY --from=ts-dev /temp/dev/typescript /typescript
COPY --from=ts-prod /temp/prod/node_modules /node_modules

# Expose ports
EXPOSE 4000 4001 6060

# Run the server
CMD ["/erpc-server"]
