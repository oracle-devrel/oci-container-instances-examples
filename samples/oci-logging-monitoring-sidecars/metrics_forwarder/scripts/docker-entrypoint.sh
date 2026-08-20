#!/usr/bin/env bash
set -euo pipefail

# Entrypoint for the OCI Monitoring sidecar image.
# It validates the required OCI settings, prepares local state directories,
# optionally starts an internal logrotate loop, and then launches the Go forwarder.

log() {
  printf '[metrics-entrypoint] %s\n' "$*"
}

is_true() {
  case "${1,,}" in
    1|true|yes|on) return 0 ;;
    *) return 1 ;;
  esac
}

require_env() {
  local name="$1"
  if [[ -z "${!name:-}" ]]; then
    log "missing required environment variable: ${name}"
    exit 1
  fi
}

render_template() {
  local dst="$2"
  # The metric file is rotated in-place with copytruncate so the generator can
  # keep writing to the same path without reopening the file.
  cat > "${dst}" <<EOF
${METRIC_FILE_PATH} {
    ${LOGROTATE_FREQUENCY}
    rotate ${LOGROTATE_ROTATE_COUNT}
    size ${LOGROTATE_SIZE}
    copytruncate
    missingok
    notifempty
    compress
}
EOF
}

start_logrotate_loop() {
  # Run logrotate on a polling loop because the container does not run cron.
  while true; do
    /usr/sbin/logrotate -v -s "${LOGROTATE_STATE_FILE}" /etc/logrotate.d/metric-file.conf
    sleep "${LOGROTATE_INTERVAL_SECONDS}"
  done
}

main() {
  # Export defaults here so the Go process sees the same resolved values that
  # the entrypoint uses for directory creation and logrotate setup.
  require_env METRIC_FILE_PATH
  require_env OCI_MONITORING_NAMESPACE
  require_env OCI_MONITORING_COMPARTMENT_ID

  export METRICS_FORWARDER_LOG_LEVEL="${METRICS_FORWARDER_LOG_LEVEL:-INFO}"
  export READ_FROM_HEAD="${READ_FROM_HEAD:-true}"
  export METRICS_FORWARDER_FLUSH_INTERVAL="${METRICS_FORWARDER_FLUSH_INTERVAL:-5s}"
  export METRICS_FORWARDER_CHUNK_LIMIT_SIZE="${METRICS_FORWARDER_CHUNK_LIMIT_SIZE:-1m}"
  export METRICS_FORWARDER_QUEUED_BATCH_LIMIT="${METRICS_FORWARDER_QUEUED_BATCH_LIMIT:-64}"
  export METRICS_FORWARDER_DISK_USAGE_LOG_INTERVAL="${METRICS_FORWARDER_DISK_USAGE_LOG_INTERVAL:-5m}"
  export LOGROTATE_ENABLED="${LOGROTATE_ENABLED:-false}"
  export LOGROTATE_FREQUENCY="${LOGROTATE_FREQUENCY:-hourly}"
  export LOGROTATE_ROTATE_COUNT="${LOGROTATE_ROTATE_COUNT:-24}"
  export LOGROTATE_SIZE="${LOGROTATE_SIZE:-50M}"

  if [[ -n "${OCI_AUTH_TYPE:-}" && "${OCI_AUTH_TYPE}" != "resource_principal" ]]; then
    log "unsupported OCI_AUTH_TYPE=${OCI_AUTH_TYPE}; this image only supports resource_principal"
    exit 1
  fi
  export OCI_AUTH_TYPE="resource_principal"

  if [[ ! -f "${METRIC_FILE_PATH}" ]]; then
    log "creating missing metric file ${METRIC_FILE_PATH}"
    mkdir -p "$(dirname "${METRIC_FILE_PATH}")"
    touch "${METRIC_FILE_PATH}"
  fi

  mkdir -p \
    "${METRICS_FORWARDER_SPOOL_DIR}" \
    "${METRICS_FORWARDER_STATE_DIR}"
  if is_true "${LOGROTATE_ENABLED}"; then
    mkdir -p "$(dirname "${LOGROTATE_STATE_FILE}")"
    render_template /etc/logrotate.d/metric-file.conf
  fi

  log "starting OCI metrics forwarder"
  log "source file: ${METRIC_FILE_PATH}"
  log "OCI auth mode: resource_principal"
  log "OCI Monitoring namespace: ${OCI_MONITORING_NAMESPACE}"
  log "OCI Monitoring compartment id: ${OCI_MONITORING_COMPARTMENT_ID}"
  log "logrotate enabled: ${LOGROTATE_ENABLED}"

  logrotate_pid=""
  if is_true "${LOGROTATE_ENABLED}"; then
    start_logrotate_loop &
    logrotate_pid="$!"
  fi
  /opt/oci-metrics-forwarder/oci-metrics-forwarder &
  forwarder_pid="$!"

  cleanup() {
    kill "${forwarder_pid}" 2>/dev/null || true
    if [[ -n "${logrotate_pid}" ]]; then
      kill "${logrotate_pid}" 2>/dev/null || true
    fi
    wait "${forwarder_pid}" 2>/dev/null || true
    if [[ -n "${logrotate_pid}" ]]; then
      wait "${logrotate_pid}" 2>/dev/null || true
    fi
  }
  trap cleanup EXIT INT TERM

  if [[ -n "${logrotate_pid}" ]]; then
    wait -n "${forwarder_pid}" "${logrotate_pid}"
    exit_code="$?"
    cleanup
    exit "${exit_code}"
  fi

  wait "${forwarder_pid}"
}

main "$@"
