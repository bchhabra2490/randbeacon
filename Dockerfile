# Build stage
FROM golang:1.22-alpine AS builder

# Install build dependencies
RUN apk add --no-cache git

# Set working directory
WORKDIR /build

# Copy go mod files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY main.go ./

# Build the application
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -installsuffix cgo -o randbeacon main.go

# Runtime stage
FROM alpine:latest

# Install ca-certificates for HTTPS connections
RUN apk --no-cache add ca-certificates tzdata

# Create app directory
WORKDIR /app

# Copy binary from builder
COPY --from=builder /build/randbeacon .

# Copy static files
COPY *.html ./

# Create directory for local secret (can be mounted as volume)
RUN mkdir -p /app/data && chmod 755 /app/data

# Expose port
EXPOSE 8080

# Set environment variables
ENV GIN_MODE=release

# Run the application
CMD ["./randbeacon"]

