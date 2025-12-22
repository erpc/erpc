# syntax = docker/dockerfile:1.4

# This Dockerfile is used to build the eRPC server image.
# Docker build stages:
#     - go-builder -> build the go binary
#     - ts-core -> Core stage for TS related stuff (just installing pnpm)
#     - ts-dev -> Install dev dependencies and compile the SDK
#     - ts-prod -> Install prod dependencies only
#     - final -> Final stage where we copy the Go binary and the TS files

# Build stage for Go
FROM golang:1.25-alpine@sha256:ac09a5f469f307e5da71e766b0bd59c9c49ea460a528cc3e6686513d64a6f1fb AS go-builder

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
    LDFLAGS="-w -s -X github.com/erpc/erpc/common.ErpcVersion=${VERSION} -X github.com/erpc/erpc/common.ErpcCommitSha=${COMMIT_SHA}"

# Build the Go binary
RUN go build -v -ldflags="$LDFLAGS" -a -installsuffix cgo -o erpc-server ./cmd/erpc/main.go && \
    go build -v -ldflags="$LDFLAGS" -a -installsuffix cgo -tags pprof -o erpc-server-pprof ./cmd/erpc/*.go

# Global typescript related image
FROM node:20-alpine@sha256:1ab6fc5a31d515dc7b6b25f6acfda2001821f2c2400252b6cb61044bd9f9ad48 AS ts-core
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

# Create symlink stage (for backwards compatibility with earlier image file structure)
FROM alpine:latest@sha256:51183f2cfa6320055da30872f211093f9ff1d3cf06f39a0bdb212314c5dc7375 AS symlink
RUN mkdir -p /root && ln -s /erpc-server /root/erpc-server

# Final stage
FROM gcr.io/distroless/static-debian12:nonroot@sha256:e8a4044e0b4ae4257efa45fc026c0bc30ad320d43bd4c1a7d5271bd241e386d0 AS final

# Copy Go binaries from go-builder
COPY --from=go-builder /build/erpc-server /
COPY --from=go-builder /build/erpc-server-pprof /

# Copy symlinked directory with preserved symlinks
COPY --from=symlink --link /root /root

# Copy TypeScript package files from ts-dev and ts-prod
COPY --from=ts-dev /temp/dev/typescript /typescript
COPY --from=ts-prod /temp/prod/node_modules /node_modules

# Expose ports
EXPOSE 4000 4001 6060

# Run the server
CMD ["/erpc-server"]
