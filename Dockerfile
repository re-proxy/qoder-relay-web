# syntax=docker/dockerfile:1

FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY web ./web
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/qoder-relay-web .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && adduser -D -H -u 10001 relay
COPY --from=build /out/qoder-relay-web /usr/local/bin/qoder-relay-web
RUN mkdir -p /data && chown relay:relay /data
USER relay
ENV RELAY_ADDR=:8317 \
    RELAY_DATA_DIR=/data \
    RELAY_DEFAULT_EDITION=domestic \
    RELAY_CHECKIN_ENABLED=true \
    RELAY_CHECKIN_HOUR=8 \
    RELAY_CHECKIN_MINUTE=30 \
    TZ=Asia/Shanghai
VOLUME ["/data"]
EXPOSE 8317
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
  CMD wget -q -O - http://127.0.0.1:8317/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/qoder-relay-web"]
