# syntax=docker/dockerfile:1.6

FROM golang:1.24-alpine AS builder
WORKDIR /app
RUN apk add --no-cache git ca-certificates
COPY go.mod go.sum ./
RUN GOTOOLCHAIN=auto go mod download
COPY . .
RUN GOTOOLCHAIN=auto CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /bin/marketflow-ai ./cmd/server

FROM alpine:3.20
# ffmpeg lays the exact poster text over motion-poster clips (internal/media/clip.go).
RUN apk add --no-cache ca-certificates tzdata ffmpeg && addgroup -S app && adduser -S app -G app
WORKDIR /app
COPY --from=builder /bin/marketflow-ai /usr/local/bin/marketflow-ai
COPY scripts/entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh
USER app
EXPOSE 4001
ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
