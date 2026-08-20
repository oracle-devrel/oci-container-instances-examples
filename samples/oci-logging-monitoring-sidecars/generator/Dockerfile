FROM oraclelinux:9-slim

# Default paths match the shared volumes mounted by the Terraform deployment.
ENV LOG_FILE_PATH=/logs/app.log \
    METRIC_FILE_PATH=/metrics/metrics.jsonl \
    HTTP_HOST=0.0.0.0 \
    HTTP_PORT=8080 \
    DEFAULT_LOG_LEVEL=INFO

RUN microdnf install -y python3 \
    && microdnf clean all

# The image contains a single self-contained Python HTTP server.
COPY http_log_server.py /usr/local/bin/http_log_server.py

RUN chmod 0755 /usr/local/bin/http_log_server.py

VOLUME ["/logs", "/metrics"]

# The generator exposes a small HTTP API for writing sample logs and metrics.
EXPOSE 8080

ENTRYPOINT ["python3", "/usr/local/bin/http_log_server.py"]
