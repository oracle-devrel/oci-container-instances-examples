#!/usr/bin/env bash
set -euo pipefail

# Entrypoint for the OCI Logging sidecar image.
# It validates the required OCI settings, prepares local state directories,
# optionally starts an internal logrotate loop, and then launches the Go forwarder.

log() {
  printf '[log-forwarder-entrypoint] %s\n' "$*"
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

  # Generate the logrotate config at runtime so container env vars fully drive
  # the rotation policy without requiring custom image builds.
  cat > "${dst}" <<EOF
${LOG_FILE_PATH} {
    ${LOGROTATE_FREQUENCY}
    rotate ${LOGROTATE_ROTATE_COUNT}
    size ${LOGROTATE_SIZE}
    su root root
    missingok
    notifempty
    dateext
    dateformat -%Y%m%d%H%M%S
    create 0644 root root
}
EOF
}

start_logrotate_loop() {
  # Run logrotate on a polling loop because the container does not run cron.
  while true; do
    /usr/sbin/logrotate -v -s "${LOGROTATE_STATE_FILE}" /etc/logrotate.d/log-file.conf
    sleep "${LOGROTATE_INTERVAL_SECONDS}"
  done
}

main() {
  # Export defaults here so the Go process sees the same resolved values that
  # the entrypoint uses for directory creation and logrotate setup.
  require_env LOG_FILE_PATH
  require_env OCI_LOG_OBJECT_ID

  export LOG_FORWARDER_LOG_LEVEL="${LOG_FORWARDER_LOG_LEVEL:-INFO}"
  export OCI_LOG_TYPE="${OCI_LOG_TYPE:-app.log}"
  export READ_FROM_HEAD="${READ_FROM_HEAD:-true}"
  export LOG_FORWARDER_FLUSH_INTERVAL="${LOG_FORWARDER_FLUSH_INTERVAL:-5s}"
  export LOG_FORWARDER_CHUNK_LIMIT_SIZE="${LOG_FORWARDER_CHUNK_LIMIT_SIZE:-1m}"
  export LOG_FORWARDER_QUEUED_BATCH_LIMIT="${LOG_FORWARDER_QUEUED_BATCH_LIMIT:-64}"
  export LOG_FORWARDER_DISK_USAGE_LOG_INTERVAL="${LOG_FORWARDER_DISK_USAGE_LOG_INTERVAL:-5m}"
  export LOGROTATE_ENABLED="${LOGROTATE_ENABLED:-false}"
  export LOGROTATE_FREQUENCY="${LOGROTATE_FREQUENCY:-hourly}"
  export LOGROTATE_ROTATE_COUNT="${LOGROTATE_ROTATE_COUNT:-24}"
  export LOGROTATE_SIZE="${LOGROTATE_SIZE:-50M}"

  if [[ -n "${OCI_AUTH_TYPE:-}" && "${OCI_AUTH_TYPE}" != "resource_principal" ]]; then
    log "unsupported OCI_AUTH_TYPE=${OCI_AUTH_TYPE}; this image only supports resource_principal"
    exit 1
  fi
  export OCI_AUTH_TYPE="resource_principal"

  if [[ ! -f "${LOG_FILE_PATH}" ]]; then
    log "creating missing log file ${LOG_FILE_PATH}"
    mkdir -p "$(dirname "${LOG_FILE_PATH}")"
    touch "${LOG_FILE_PATH}"
  fi

  mkdir -p \
    "${LOG_FORWARDER_SPOOL_DIR}" \
    "${LOG_FORWARDER_STATE_DIR}"
  if is_true "${LOGROTATE_ENABLED}"; then
    mkdir -p "$(dirname "${LOGROTATE_STATE_FILE}")"
    render_template /etc/logrotate.d/log-file.conf
  fi

  log "starting OCI log forwarder"
  log "source file: ${LOG_FILE_PATH}"
  log "OCI auth mode: resource_principal"
  log "OCI log object id: ${OCI_LOG_OBJECT_ID}"
  log "logrotate enabled: ${LOGROTATE_ENABLED}"

  logrotate_pid=""
  if is_true "${LOGROTATE_ENABLED}"; then
    start_logrotate_loop &
    logrotate_pid="$!"
  fi
  /opt/oci-log-forwarder/oci-log-forwarder &
  log_forwarder_pid="$!"

  cleanup() {
    kill "${log_forwarder_pid}" 2>/dev/null || true
    if [[ -n "${logrotate_pid}" ]]; then
      kill "${logrotate_pid}" 2>/dev/null || true
    fi
    wait "${log_forwarder_pid}" 2>/dev/null || true
    if [[ -n "${logrotate_pid}" ]]; then
      wait "${logrotate_pid}" 2>/dev/null || true
    fi
  }
  trap cleanup EXIT INT TERM

  if [[ -n "${logrotate_pid}" ]]; then
    wait -n "${log_forwarder_pid}" "${logrotate_pid}"
    exit_code="$?"
    cleanup
    exit "${exit_code}"
  fi

  wait "${log_forwarder_pid}"
}

main "$@"
