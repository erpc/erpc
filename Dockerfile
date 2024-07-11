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

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o erpc-server ./cmd/erpc/main.go

# Final stage
FROM alpine:3.18

# Set the working directory
WORKDIR /root/

# Copy the binary from the builder stage
COPY --from=builder /app/erpc-server .

# Expose the port the app runs on
EXPOSE 8080

# Run the binary
CMD ["./erpc-server"]