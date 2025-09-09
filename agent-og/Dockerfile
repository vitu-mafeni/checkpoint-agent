# Build stage
FROM golang:1.21-alpine AS builder

# Install dependencies
RUN apk add --no-cache git ca-certificates

# Set workdir
WORKDIR /app

# Copy go.mod and go.sum first (for caching)
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux go build -o checkpoint-agent .

# Final stage
FROM alpine:latest

# Install certificates for TLS
RUN apk add --no-cache ca-certificates

# Set working directory
WORKDIR /app

# Copy the binary
COPY --from=builder /app/checkpoint-agent .

# Expose volume for kubelet checkpoints
VOLUME ["/var/lib/kubelet/checkpoints"]

# Environment variables (can be overridden in the DaemonSet)
ENV CHECKPOINT_DIR=/var/lib/kubelet/checkpoints \
    MINIO_ENDPOINT=minio:9000 \
    MINIO_ACCESS_KEY=minio-access-key \
    MINIO_SECRET_KEY=minio-secret-key \
    MINIO_BUCKET=checkpoints \
    PULL_INTERVAL=5m

# Run the agent
ENTRYPOINT ["./checkpoint-agent"]
