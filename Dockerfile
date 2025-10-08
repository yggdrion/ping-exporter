FROM golang:1.23-alpine AS builder

WORKDIR /build

# Install build dependencies
RUN apk add --no-cache git

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY *.go ./

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o ping-exporter .

# Final stage
FROM alpine:latest

RUN apk --no-cache add ca-certificates tzdata iputils

WORKDIR /app

# Copy binary from builder
COPY --from=builder /build/ping-exporter .

# Copy default config
COPY targets.json .

# Expose metrics port
EXPOSE 9090

# Run as non-root user
RUN addgroup -g 1000 appuser && \
    adduser -D -u 1000 -G appuser appuser && \
    chown -R appuser:appuser /app

USER appuser

ENTRYPOINT ["/app/ping-exporter"]
