#!/usr/bin/env bash
set -euo pipefail

# Build and publish the three sample images to an OCI-compatible registry.

usage() {
  cat <<'EOF'
Usage:
  scripts/push-ocir-images.sh <ocir_registry> <namespace> [tag]

Example:
  scripts/push-ocir-images.sh uk-london-1.ocir.io axwtwdagdjcl latest

This script builds and pushes:
  <ocir_registry>/<namespace>/oci-generator:<tag>
  <ocir_registry>/<namespace>/oci-log-forwarder:<tag>
  <ocir_registry>/<namespace>/oci-metrics-forwarder:<tag>

Prerequisite:
  docker must already be authenticated to the target OCIR registry.
EOF
}

log() {
  printf '[push-ocir-images] %s\n' "$*"
}

require_command() {
  local command_name="$1"
  if ! command -v "${command_name}" >/dev/null 2>&1; then
    printf 'missing required command: %s\n' "${command_name}" >&2
    exit 1
  fi
}

build_and_push() {
  local image_name="$1"
  local context_dir="$2"
  local full_image_ref="${OCIR_REGISTRY}/${NAMESPACE}/${image_name}:${TAG}"

  # Each image is built from its local sample directory so the script can be
  # run from anywhere inside or outside the repository.
  log "building ${full_image_ref} from ${context_dir}"
  docker build -t "${full_image_ref}" "${REPO_ROOT}/${context_dir}"

  log "pushing ${full_image_ref}"
  docker push "${full_image_ref}"
}

if [[ $# -lt 2 || $# -gt 3 ]]; then
  usage
  exit 1
fi

require_command docker

OCIR_REGISTRY="${1%/}"
NAMESPACE="${2}"
TAG="${3:-latest}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

build_and_push "oci-generator" "generator"
build_and_push "oci-log-forwarder" "log_forwarder"
build_and_push "oci-metrics-forwarder" "metrics_forwarder"

log "finished pushing all images"
