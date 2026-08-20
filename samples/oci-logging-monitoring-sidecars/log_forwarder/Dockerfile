FROM golang:1.24-alpine AS builder

# Build a static binary so the runtime image stays small and simple.
WORKDIR /src

COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY app/ app/
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o /out/oci-log-forwarder ./app

FROM alpine:3.21

# Runtime defaults keep spool, state, and logrotate metadata on writable paths.
ENV LOG_FORWARDER_SPOOL_DIR=/var/lib/oci-log-forwarder/spool \
    LOG_FORWARDER_STATE_DIR=/var/lib/oci-log-forwarder/state \
    LOGROTATE_STATE_FILE=/var/lib/logrotate/logrotate.status \
    LOGROTATE_INTERVAL_SECONDS=60

RUN apk add --no-cache \
      bash \
      ca-certificates \
      gzip \
      logrotate

RUN mkdir -p \
      /etc/logrotate.d \
      /var/lib/oci-log-forwarder/spool \
      /var/lib/oci-log-forwarder/state \
      /var/lib/logrotate \
      /opt/oci-log-forwarder

COPY --from=builder /out/oci-log-forwarder /opt/oci-log-forwarder/oci-log-forwarder
COPY scripts/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh

RUN chmod 0755 /usr/local/bin/docker-entrypoint.sh /opt/oci-log-forwarder/oci-log-forwarder

# The container shares the source log volume plus persistent forwarder state.
VOLUME ["/logs", "/var/lib/oci-log-forwarder"]

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
