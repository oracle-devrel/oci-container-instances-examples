# OCI Container Instance Logging And Metrics

This sample shows how to run a generator container with optional sidecars that forward shared-file logs to OCI Logging and shared-file metrics to OCI Monitoring.

## Get The Example Working

### 1. Build and push the images

From this directory:

```bash
docker build -t oci-generator ./generator
docker build -t oci-log-forwarder ./log_forwarder
docker build -t oci-metrics-forwarder ./metrics_forwarder
./scripts/push-ocir-images.sh <ocir-registry> <namespace> [tag]
```

Example:

```bash
./scripts/push-ocir-images.sh uk-london-1.ocir.io axwtwdagdjcl latest
```

The target registry must already be reachable by OCI Container Instances, and `docker login` must already be done for OCIR.

### 2. Deploy the sample with Terraform

The quickest path is the Terraform under `container_instance/`.

```bash
cd container_instance
cp terraform.tfvars.example terraform.tfvars
```

Set at least these variables in `terraform.tfvars`:

- `tenancy_ocid`
- `compartment_id`
- `region`
- `availability_domain`
- `generator_image_url`

Set these when you want the sidecars enabled:

- `enable_log_forwarder = true`
- `log_forwarder_image_url = "<your log forwarder image>"`
- `enable_metrics_forwarder = true`
- `metrics_forwarder_image_url = "<your metrics forwarder image>"`

Then apply:

```bash
terraform init
terraform plan -out tfplan
terraform apply tfplan
```

The Terraform creates the network, IAM policy and dynamic group, optional OCI Logging resources, and the container instance runtime. See [`container_instance/README.md`](container_instance/README.md) for the full deployment inputs and outputs.

### 3. Send test traffic

After the container instance is up, send logs to the generator:

```bash
curl -X POST http://<generator-host>:8080/log \
  -H 'Content-Type: application/json' \
  -d '{"level":"INFO","message":"hello from generator"}'
```

Send metrics when the metrics sidecar is enabled:

```bash
curl -X POST http://<generator-host>:8080/metric \
  -H 'Content-Type: application/json' \
  -d '{"name":"request_count","value":1,"dimensions":{"service":"generator"}}'
```

Optional traffic generators:

```bash
curl -X POST http://<generator-host>:8080/random/logs \
  -H 'Content-Type: application/json' \
  -d '{"enabled":true}'

curl -X POST http://<generator-host>:8080/random/metrics \
  -H 'Content-Type: application/json' \
  -d '{"enabled":true}'
```

### 4. Know the local-runtime limitation

Both sidecars are resource-principal-only. Their OCI API calls are expected to work in OCI Container Instances, not on a plain local Docker host.

The generator can still be run locally for file-generation tests:

```bash
docker run --rm \
  -e LOG_FILE_PATH=/logs/app.log \
  -e METRIC_FILE_PATH=/metrics/metrics.jsonl \
  -e HTTP_PORT=8080 \
  -p 8080:8080 \
  -v "$PWD/logs:/logs" \
  -v "$PWD/metrics:/metrics" \
  oci-generator
```

## Environment Variables

### Logging Sidecar

| Variable | Required | Group | Default | Purpose |
| --- | --- | --- | --- | --- |
| `LOG_FILE_PATH` | yes | Required | none | Shared log file that the forwarder tails. |
| `OCI_LOG_OBJECT_ID` | yes | Required | none | OCI custom log OCID that receives forwarded entries. |
| `OCI_AUTH_TYPE` | no | OCI auth and target | `resource_principal` | Compatibility guard; any other value is rejected. |
| `OCI_LOG_TYPE` | no | OCI auth and target | `app.log` | Log batch type sent to OCI Logging. |
| `READ_FROM_HEAD` | no | Read behavior | `true` | Reads existing file content on first startup. |
| `LOG_FORWARDER_LOG_LEVEL` | no | Read behavior | `INFO` | Forwarder container log level. |
| `LOG_FORWARDER_FLUSH_INTERVAL` | no | Batching and backpressure | `5s` | Maximum delay before the next queued batch is sent. |
| `LOG_FORWARDER_CHUNK_LIMIT_SIZE` | no | Batching and backpressure | `1m` | Maximum batch payload before the reader flushes immediately. |
| `LOG_FORWARDER_QUEUED_BATCH_LIMIT` | no | Batching and backpressure | `64` | Maximum number of pending spool files before reads pause. |
| `OCI_MAX_BATCH_ENTRIES` | no | Batching and backpressure | `1000` | Maximum log lines per OCI Logging request. |
| `OCI_MAX_ENTRY_SIZE_BYTES` | no | Batching and backpressure | `900000` | Maximum size of a single line before truncation. |
| `LOG_FORWARDER_STATE_DIR` | no | Local state and spool | `/var/lib/oci-log-forwarder/state` | Directory used for checkpoint state. |
| `LOG_FORWARDER_SPOOL_DIR` | no | Local state and spool | `/var/lib/oci-log-forwarder/spool` | Directory used for on-disk pending batches. |
| `LOG_QUEUE_DIR` | no | Local state and spool | `${LOG_FORWARDER_SPOOL_DIR}` | Explicit spool-path override. |
| `LOG_FORWARDER_DISK_USAGE_LOG_INTERVAL` | no | Local state and spool | `5m` | How often the container logs total size of the source file plus rotated siblings. |
| `LOGROTATE_ENABLED` | no | Rotation | `false` | Starts the internal logrotate loop when enabled. |
| `LOGROTATE_FREQUENCY` | no | Rotation | `hourly` | Logrotate cadence keyword. |
| `LOGROTATE_SIZE` | no | Rotation | `50M` | Rotates after the file reaches this size. |
| `LOGROTATE_ROTATE_COUNT` | no | Rotation | `24` | Number of rotated files to keep. |
| `LOGROTATE_INTERVAL_SECONDS` | no | Rotation | `60` | How often the entrypoint invokes logrotate. |

### Monitoring Sidecar

| Variable | Required | Group | Default | Purpose |
| --- | --- | --- | --- | --- |
| `METRIC_FILE_PATH` | yes | Required | none | Shared JSON-lines metric file that the forwarder tails. |
| `OCI_MONITORING_NAMESPACE` | yes | Required | none | Default OCI Monitoring namespace for emitted metrics. |
| `OCI_MONITORING_COMPARTMENT_ID` | yes | Required | none | Default target compartment OCID for emitted metrics. |
| `OCI_REGION` | no | OCI auth and target | runtime-derived | Region used to derive the telemetry ingestion endpoint. |
| `OCI_MONITORING_RESOURCE_GROUP` | no | OCI auth and target | none | Optional default resource group. |
| `OCI_MONITORING_INGESTION_ENDPOINT` | no | OCI auth and target | derived from `OCI_REGION` | Explicit telemetry ingestion endpoint override. |
| `OCI_AUTH_TYPE` | no | OCI auth and target | `resource_principal` | Compatibility guard; any other value is rejected. |
| `READ_FROM_HEAD` | no | Read behavior | `true` | Reads existing file content on first startup. |
| `METRICS_FORWARDER_LOG_LEVEL` | no | Read behavior | `INFO` | Forwarder container log level. |
| `METRICS_FORWARDER_FLUSH_INTERVAL` | no | Batching and backpressure | `5s` | Maximum delay before the next queued metric batch is sent. |
| `METRICS_FORWARDER_CHUNK_LIMIT_SIZE` | no | Batching and backpressure | `1m` | Maximum batch payload before the reader flushes immediately. |
| `METRICS_FORWARDER_QUEUED_BATCH_LIMIT` | no | Batching and backpressure | `64` | Maximum number of pending spool files before reads pause. |
| `METRIC_POLL_INTERVAL_SECONDS` | no | Batching and backpressure | `1` | Poll interval for new metric lines. |
| `OCI_MAX_BATCH_ENTRIES` | no | Batching and backpressure | `50` | Maximum metric records per OCI Monitoring request. |
| `METRICS_FORWARDER_STATE_DIR` | no | Local state and spool | `/var/lib/oci-metrics-forwarder/state` | Directory used for checkpoint state. |
| `METRICS_FORWARDER_SPOOL_DIR` | no | Local state and spool | `/var/lib/oci-metrics-forwarder/spool` | Directory used for on-disk pending batches. |
| `METRIC_QUEUE_DIR` | no | Local state and spool | `${METRICS_FORWARDER_SPOOL_DIR}` | Explicit spool-path override. |
| `METRIC_STATE_FILE` | no | Local state and spool | `${METRICS_FORWARDER_STATE_DIR}/input.json` | Explicit checkpoint file override. |
| `METRICS_FORWARDER_DISK_USAGE_LOG_INTERVAL` | no | Local state and spool | `5m` | How often the container logs total size of the source file plus rotated siblings. |
| `LOGROTATE_ENABLED` | no | Rotation | `false` | Starts the internal logrotate loop when enabled. |
| `LOGROTATE_FREQUENCY` | no | Rotation | `hourly` | Logrotate cadence keyword. |
| `LOGROTATE_SIZE` | no | Rotation | `50M` | Rotates after the file reaches this size. |
| `LOGROTATE_ROTATE_COUNT` | no | Rotation | `24` | Number of rotated files to keep. |
| `LOGROTATE_INTERVAL_SECONDS` | no | Rotation | `60` | How often the entrypoint invokes logrotate. |

## Overall Architecture

The sample has four parts:

- `generator/`: HTTP service that writes application logs and JSON-line metrics into shared files.
- `log_forwarder/`: optional sidecar that tails the shared log file, spools batches to disk, and sends them to OCI Logging.
- `metrics_forwarder/`: optional sidecar that tails the shared metric file, validates metric records, spools batches to disk, and sends them to OCI Monitoring.
- `container_instance/`: Terraform that provisions the network, IAM, optional OCI Logging resources, and the OCI Container Instance runtime.

Runtime flow:

1. The generator writes `/mnt/logs/app.log` and `/mnt/metrics/metrics.jsonl`.
2. The sidecars mount the same shared volumes and track offsets by inode, so rename-based rotations can still be drained correctly.
3. Each sidecar persists checkpoints under its state directory and writes pending batches into its spool directory before attempting OCI API calls.
4. The log sidecar pushes to OCI Logging by using the injected resource principal and the custom log OCID.
5. The metrics sidecar pushes to the OCI telemetry ingestion endpoint by using the injected resource principal plus namespace and compartment defaults.

## Rotation Behavior

### Log rotation

- The log forwarder entrypoint can run an internal `logrotate` loop when `LOGROTATE_ENABLED=true`.
- It uses rename-and-create rotation with `dateext`, so the active file name stays stable while rotated siblings are preserved.
- The forwarder tracks files by inode and offset, which lets it continue draining renamed files after rotation.
- Pending log batches are written to the spool directory before they are sent to OCI Logging, so transient OCI failures do not lose already-read lines.

### Metric rotation

- The metrics forwarder entrypoint can also run an internal `logrotate` loop when `LOGROTATE_ENABLED=true`.
- Its generated logrotate config uses `copytruncate`, which keeps the metric producer writing to the same file path while the rotated copy is archived.
- The forwarder still tracks inode and offsets, persists checkpoints, and drains queued metric batches from disk before exit.
- Metrics are validated when read from the shared file; invalid JSON lines are dropped with warnings instead of blocking the pipeline.

## Repository Layout

```text
generator/          generator container image
log_forwarder/      OCI Logging sidecar image
metrics_forwarder/  OCI Monitoring sidecar image
container_instance/ Terraform deployment
scripts/            helper scripts for image publishing
```
