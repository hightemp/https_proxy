# syntax=docker/dockerfile:1

FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY main.go ./
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/https_proxy main.go

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tini \
    && adduser -D -H -u 10001 proxy
COPY --from=builder /out/https_proxy /usr/bin/https_proxy
COPY config.docker.yaml /etc/https_proxy/config.yaml
EXPOSE 8080 8443
USER proxy
ENTRYPOINT ["/sbin/tini","--","/usr/bin/https_proxy"]
CMD ["-config","/etc/https_proxy/config.yaml"]
