# Multi-stage build for SLO Metric Generator
# Stage 1: Build the Go application
FROM golang:1.23-alpine AS builder

# Install build dependencies
RUN apk add --no-cache git

WORKDIR /app

# Set GOTOOLCHAIN to allow Go to use newer versions if needed
ENV GOTOOLCHAIN=auto

# Copy go mod files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY *.go ./
COPY static/ ./static/

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o slo-metric-generator .

# Stage 2: Create minimal runtime image
FROM alpine:latest

# Install ca-certificates for HTTPS and docker client for container management
RUN apk --no-cache add ca-certificates docker-cli

WORKDIR /app

# Copy the binary from builder
COPY --from=builder /app/slo-metric-generator .
COPY --from=builder /app/static ./static

# Expose the application port
EXPOSE 8080

# Run the application
CMD ["./slo-metric-generator"]
