# Build stage
FROM golang:1.22-alpine AS builder

# Set the working directory
WORKDIR /app

# Copy go mod and sum files
COPY go.mod go.sum ./

# Download all dependencies
RUN go mod download

# Copy the source code
COPY . .

# Build the application without pprof
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o erpc-server ./cmd/erpc/main.go

# Build the application with pprof
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -tags pprof -o erpc-server-pprof ./cmd/erpc/*.go

# Final stage
FROM alpine:3.18

# Set the working directory
WORKDIR /root/

# Copy the binaries from the builder stage
COPY --from=builder /app/erpc-server .
COPY --from=builder /app/erpc-server-pprof .

# 8080 -> erpc
# 6060 -> pprof (optional)
EXPOSE 8080 6060

# Use an environment variable to determine which binary to run
ENV PPROF=false

# Run the appropriate binary based on the environment variable
CMD if [ "$PPROF" = "true" ]; then ./erpc-server-pprof; else ./erpc-server; fi