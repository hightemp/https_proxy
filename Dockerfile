# syntax=docker/dockerfile:1

FROM golang:1.26.5-alpine AS builder
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/https_proxy ./cmd/https_proxy

FROM alpine:3.20
LABEL org.opencontainers.image.source="https://github.com/hightemp/https_proxy"
RUN apk add --no-cache ca-certificates tini \
    && adduser -D -H -u 10001 proxy
COPY --from=builder /out/https_proxy /usr/bin/https_proxy
COPY config.docker.yaml /etc/https_proxy/config.yaml
EXPOSE 8080 8443
USER proxy
ENTRYPOINT ["/sbin/tini","--","/usr/bin/https_proxy"]
CMD ["-config","/etc/https_proxy/config.yaml"]
