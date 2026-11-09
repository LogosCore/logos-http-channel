# syntax=docker/dockerfile:1.7

# The build stage runs on the builder's own platform (BUILDPLATFORM) and
# cross-compiles for each target (TARGETOS, TARGETARCH): CI builds amd64 and
# arm64 on an amd64 runner, where compiling under QEMU took about two
# minutes for arm64 against twenty seconds natively. The final stage runs no
# command, so arm64 needs no emulation at all. Docker's classic builder
# (compose without the buildx plugin) sets none of the three: BUILDPLATFORM
# defaults to amd64 below, and an empty GOOS and GOARCH build for the host.
ARG BUILDPLATFORM=linux/amd64
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
# VERSION is reported to core at registration.
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -ldflags "-X main.version=${VERSION}" -o /out/http-channel ./cmd/http-channel

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
