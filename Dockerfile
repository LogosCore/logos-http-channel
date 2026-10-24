# syntax=docker/dockerfile:1.7

# No GOARCH: the build stage runs as the target platform (QEMU in CI), so the
# binary always matches the image's architecture.
FROM golang:1.25-alpine AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# VERSION is reported to core at registration.
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION}" -o /out/http-channel ./cmd/http-channel

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app

COPY --from=builder /out/http-channel /app/http-channel
# The binary is configured entirely from the environment (see
# internal/config.LoadFromEnv).
# 8080 is the client-facing port. The health server (HEALTH_LISTEN_ADDR,
# default :8081) is intentionally not EXPOSEd so `docker run -P` won't
# publish it; reach it from the Docker network only. Never publish it.
EXPOSE 8080

ENTRYPOINT ["/app/http-channel"]
