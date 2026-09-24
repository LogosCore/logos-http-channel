# syntax=docker/dockerfile:1.7

FROM golang:1.25-alpine AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/http-channel ./cmd/http-channel

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app

COPY --from=builder /out/http-channel /app/http-channel
COPY profiles /app/profiles

# The binary is configured entirely from the environment (see
# internal/config.LoadFromEnv); the only thing it reads off disk is the
# transposition profiles directory.
ENV PROFILES_DIR=/app/profiles
EXPOSE 8080

ENTRYPOINT ["/app/http-channel"]
