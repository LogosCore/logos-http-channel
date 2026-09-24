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
COPY configs /app/configs

ENV CONFIG_FILE=/app/configs/channel.example.yaml
EXPOSE 8080

ENTRYPOINT ["/app/http-channel"]
