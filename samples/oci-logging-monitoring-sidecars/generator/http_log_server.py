#!/usr/bin/env python3
"""HTTP generator for the sidecar sample.

This container writes application logs and JSON-line metrics into shared files
that can be consumed by the log and metrics forwarder sidecars.
"""

from __future__ import annotations

import json
import os
import random
import threading
import time
from datetime import datetime, timezone
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path


# Runtime configuration for the shared-file producer.
LOG_FILE_PATH = Path(os.environ.get("LOG_FILE_PATH", "/logs/app.log"))
METRIC_FILE_PATH = Path(os.environ.get("METRIC_FILE_PATH", "/metrics/metrics.jsonl"))
HOST = os.environ.get("HTTP_HOST", "0.0.0.0")
PORT = int(os.environ.get("HTTP_PORT", "8080"))
DEFAULT_LEVEL = os.environ.get("DEFAULT_LOG_LEVEL", "INFO")
DEFAULT_METRIC_NAMESPACE = os.environ.get("DEFAULT_METRIC_NAMESPACE", "generator")
LOG_FORMAT = os.environ.get(
    "LOG_FORMAT",
    "{timestamp} level={level} message={message}",
)
RANDOM_LOG_INTERVAL_SECONDS = float(os.environ.get("RANDOM_LOG_INTERVAL_SECONDS", "5"))
RANDOM_METRIC_INTERVAL_SECONDS = float(os.environ.get("RANDOM_METRIC_INTERVAL_SECONDS", "5"))
ENABLE_RANDOM_LOGS = os.environ.get("ENABLE_RANDOM_LOGS", "false").strip().lower() in {"1", "true", "yes", "on"}
ENABLE_RANDOM_METRICS = os.environ.get("ENABLE_RANDOM_METRICS", "false").strip().lower() in {"1", "true", "yes", "on"}

LOG_WRITE_LOCK = threading.Lock()
METRIC_WRITE_LOCK = threading.Lock()
STATE_LOCK = threading.Lock()
RANDOM_LOGS_ENABLED = ENABLE_RANDOM_LOGS
RANDOM_METRICS_ENABLED = ENABLE_RANDOM_METRICS


def utc_timestamp() -> str:
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def parse_enabled_flag(value: object) -> bool:
    if isinstance(value, bool):
        return value
    if isinstance(value, str):
        return value.strip().lower() in {"1", "true", "yes", "on"}
    if isinstance(value, (int, float)):
        return bool(value)
    raise ValueError("enabled must be a boolean")


def ensure_output_files() -> None:
    """Create the shared output files before the HTTP server starts."""
    LOG_FILE_PATH.parent.mkdir(parents=True, exist_ok=True)
    LOG_FILE_PATH.touch(exist_ok=True)
    METRIC_FILE_PATH.parent.mkdir(parents=True, exist_ok=True)
    METRIC_FILE_PATH.touch(exist_ok=True)


def append_log(level: str, message: str) -> str:
    """Append a single formatted application log line."""
    line = LOG_FORMAT.format(timestamp=utc_timestamp(), level=level, message=message)
    with LOG_WRITE_LOCK:
        with LOG_FILE_PATH.open("a", encoding="utf-8") as handle:
            handle.write(f"{line}\n")
    return line


def append_metric(metric: dict) -> dict:
    """Append one metric record as a compact JSON line."""
    payload = {key: value for key, value in dict(metric).items() if value is not None}
    payload.setdefault("timestamp", datetime.now(timezone.utc).isoformat())
    payload.setdefault("namespace", DEFAULT_METRIC_NAMESPACE)
    payload.setdefault("dimensions", {})
    payload.setdefault("metadata", {})

    with METRIC_WRITE_LOCK:
        with METRIC_FILE_PATH.open("a", encoding="utf-8") as handle:
            handle.write(f"{json.dumps(payload, separators=(',', ':'))}\n")
    return payload


def random_log_loop() -> None:
    """Emit sample logs at a fixed cadence when the toggle is enabled."""
    while True:
        with STATE_LOCK:
            enabled = RANDOM_LOGS_ENABLED
        if enabled:
            level = random.choice(["DEBUG", "INFO", "WARN", "ERROR"])
            message = f"random-log-{random.randint(1000, 9999)}"
            line = append_log(level=level, message=message)
            print(f"[generator] emitted random log: {line}", flush=True)
        time.sleep(RANDOM_LOG_INTERVAL_SECONDS)


def random_metric_loop() -> None:
    """Emit sample metrics at a fixed cadence when the toggle is enabled."""
    while True:
        with STATE_LOCK:
            enabled = RANDOM_METRICS_ENABLED
        if enabled:
            metric = append_metric(
                {
                    "name": random.choice(["request_count", "cpu_load", "queue_depth"]),
                    "value": round(random.uniform(1, 100), 2),
                    "dimensions": {
                        "service": "generator",
                        "source": "random",
                    },
                }
            )
            print(f"[generator] emitted random metric: {json.dumps(metric)}", flush=True)
        time.sleep(RANDOM_METRIC_INTERVAL_SECONDS)


class LogRequestHandler(BaseHTTPRequestHandler):
    """Expose write and toggle endpoints for the sample generator."""
    server_version = "oci-generator/1.1"

    def _send_json(self, status: int, payload: dict) -> None:
        body = json.dumps(payload).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _read_json_body(self) -> dict | None:
        try:
            content_length = int(self.headers.get("Content-Length", "0"))
        except ValueError:
            self._send_json(HTTPStatus.BAD_REQUEST, {"error": "invalid content-length"})
            return None

        raw_body = self.rfile.read(content_length)
        try:
            payload = json.loads(raw_body or b"{}")
        except json.JSONDecodeError:
            self._send_json(HTTPStatus.BAD_REQUEST, {"error": "body must be valid JSON"})
            return None

        if not isinstance(payload, dict):
            self._send_json(HTTPStatus.BAD_REQUEST, {"error": "body must be a JSON object"})
            return None

        return payload

    def _handle_log_write(self) -> None:
        payload = self._read_json_body()
        if payload is None:
            return

        level = str(payload.get("level", DEFAULT_LEVEL)).strip() or DEFAULT_LEVEL
        message = str(payload.get("message", "")).strip()
        if not message:
            self._send_json(HTTPStatus.BAD_REQUEST, {"error": "message is required"})
            return

        line = append_log(level=level.upper(), message=message)
        self._send_json(
            HTTPStatus.CREATED,
            {
                "status": "written",
                "line": line,
                "log_file_path": str(LOG_FILE_PATH),
            },
        )

    def _handle_metric_write(self) -> None:
        payload = self._read_json_body()
        if payload is None:
            return

        name = str(payload.get("name", "")).strip()
        if not name:
            self._send_json(HTTPStatus.BAD_REQUEST, {"error": "name is required"})
            return

        if "value" not in payload:
            self._send_json(HTTPStatus.BAD_REQUEST, {"error": "value is required"})
            return

        try:
            value = float(payload["value"])
        except (TypeError, ValueError):
            self._send_json(HTTPStatus.BAD_REQUEST, {"error": "value must be numeric"})
            return

        dimensions = payload.get("dimensions", {})
        metadata = payload.get("metadata", {})
        if not isinstance(dimensions, dict):
            self._send_json(HTTPStatus.BAD_REQUEST, {"error": "dimensions must be an object"})
            return
        if not isinstance(metadata, dict):
            self._send_json(HTTPStatus.BAD_REQUEST, {"error": "metadata must be an object"})
            return

        metric = append_metric(
            {
                "name": name,
                "value": value,
                "timestamp": payload.get("timestamp"),
                "dimensions": dimensions,
                "metadata": metadata,
                "resource_group": payload.get("resource_group"),
                "namespace": payload.get("namespace", DEFAULT_METRIC_NAMESPACE),
                "compartment_id": payload.get("compartment_id"),
            }
        )
        self._send_json(
            HTTPStatus.CREATED,
            {
                "status": "written",
                "metric": metric,
                "metric_file_path": str(METRIC_FILE_PATH),
            },
        )

    def _handle_random_toggle(self, kind: str) -> None:
        payload = self._read_json_body()
        if payload is None:
            return

        if "enabled" not in payload:
            self._send_json(HTTPStatus.BAD_REQUEST, {"error": "enabled is required"})
            return

        try:
            enabled = parse_enabled_flag(payload["enabled"])
        except ValueError as exc:
            self._send_json(HTTPStatus.BAD_REQUEST, {"error": str(exc)})
            return
        with STATE_LOCK:
            global RANDOM_LOGS_ENABLED, RANDOM_METRICS_ENABLED
            if kind == "logs":
                RANDOM_LOGS_ENABLED = enabled
            else:
                RANDOM_METRICS_ENABLED = enabled

        self._send_json(
            HTTPStatus.OK,
            {
                "status": "updated",
                "random_logs_enabled": RANDOM_LOGS_ENABLED,
                "random_metrics_enabled": RANDOM_METRICS_ENABLED,
            },
        )

    def log_message(self, fmt: str, *args) -> None:
        return

    def do_GET(self) -> None:
        if self.path == "/health":
            self._send_json(
                HTTPStatus.OK,
                {
                    "status": "ok",
                    "log_file_path": str(LOG_FILE_PATH),
                    "metric_file_path": str(METRIC_FILE_PATH),
                    "random_logs_enabled": RANDOM_LOGS_ENABLED,
                    "random_metrics_enabled": RANDOM_METRICS_ENABLED,
                },
            )
            return

        self._send_json(HTTPStatus.NOT_FOUND, {"error": "not found"})

    def do_POST(self) -> None:
        if self.path == "/log":
            self._handle_log_write()
            return

        if self.path == "/metric":
            self._handle_metric_write()
            return

        if self.path == "/random/logs":
            self._handle_random_toggle("logs")
            return

        if self.path == "/random/metrics":
            self._handle_random_toggle("metrics")
            return

        self._send_json(HTTPStatus.NOT_FOUND, {"error": "not found"})


def main() -> None:
    ensure_output_files()

    threading.Thread(target=random_log_loop, daemon=True).start()
    threading.Thread(target=random_metric_loop, daemon=True).start()

    print(f"[generator] listening on {HOST}:{PORT}", flush=True)
    print(f"[generator] writing logs to {LOG_FILE_PATH}", flush=True)
    print(f"[generator] writing metrics to {METRIC_FILE_PATH}", flush=True)
    print(f"[generator] random logs enabled: {RANDOM_LOGS_ENABLED}", flush=True)
    print(f"[generator] random metrics enabled: {RANDOM_METRICS_ENABLED}", flush=True)

    server = ThreadingHTTPServer((HOST, PORT), LogRequestHandler)
    server.serve_forever()


if __name__ == "__main__":
    main()
