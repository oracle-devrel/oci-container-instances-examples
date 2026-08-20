FROM golang:1.24-alpine AS builder

# Build a static binary so the runtime image stays small and simple.
WORKDIR /src

COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

COPY app/ app/
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o /out/oci-metrics-forwarder ./app

FROM alpine:3.21

# Runtime defaults keep spool, state, and logrotate metadata on writable paths.
ENV METRICS_FORWARDER_SPOOL_DIR=/var/lib/oci-metrics-forwarder/spool \
    METRICS_FORWARDER_STATE_DIR=/var/lib/oci-metrics-forwarder/state \
    LOGROTATE_STATE_FILE=/var/lib/logrotate/logrotate.status \
    LOGROTATE_INTERVAL_SECONDS=60

RUN apk add --no-cache \
      bash \
      ca-certificates \
      gzip \
      logrotate

RUN mkdir -p \
      /etc/logrotate.d \
      /var/lib/oci-metrics-forwarder/spool \
      /var/lib/oci-metrics-forwarder/state \
      /var/lib/logrotate \
      /opt/oci-metrics-forwarder

COPY --from=builder /out/oci-metrics-forwarder /opt/oci-metrics-forwarder/oci-metrics-forwarder
COPY scripts/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh

RUN chmod 0755 /usr/local/bin/docker-entrypoint.sh /opt/oci-metrics-forwarder/oci-metrics-forwarder

# The container shares the source metric volume plus persistent forwarder state.
VOLUME ["/metrics", "/var/lib/oci-metrics-forwarder"]

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
