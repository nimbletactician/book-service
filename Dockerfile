# Build stage
FROM golang:1.22-alpine AS builder

# Enable Go's build cache
ENV GOCACHE=/go-cache
ENV GOMODCACHE=/go-mod-cache

WORKDIR /app

# Copy only dependency files first
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go-mod-cache \
    go mod download

# Copy source and build
COPY . .
RUN --mount=type=cache,target=/go-cache \
    --mount=type=cache,target=/go-mod-cache \
    CGO_ENABLED=0 go build -o bookstore

# Final stage
FROM alpine:3.18
WORKDIR /app
COPY --from=builder /app/bookstore .
EXPOSE 8080
CMD ["./bookstore"]