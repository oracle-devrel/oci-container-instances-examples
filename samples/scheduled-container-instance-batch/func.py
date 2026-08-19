"""Scheduled OCI Container Instance batch lifecycle function.

The function is intentionally invoked by OCI Resource Scheduler instead of
sleeping until a deadline.  This keeps the function short-lived and lets OCI
reliably deliver both the create and cleanup invocations.
"""

import io
import json
import math
import os
import re
import time
import uuid
from datetime import datetime, timezone

import oci


SAFE_ID = re.compile(r"^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$")
DEFAULT_MAX_RETRIES = 3
DEFAULT_CREATE_REQUESTS_PER_SECOND = 1
DEFAULT_DELETE_REQUESTS_PER_SECOND = 1
DEFAULT_CREATE_REQUESTS_PER_MINUTE = 10
DEFAULT_DELETE_REQUESTS_PER_MINUTE = 10


class RequestPacer:
    """Limits API calls to the stricter of a per-second and per-minute rate."""

    def __init__(self, requests_per_second, requests_per_minute):
        self.interval = max(1 / requests_per_second, 60 / requests_per_minute)
        self.next_request_at = None

    def wait(self):
        if self.next_request_at is not None:
            delay = self.next_request_at - time.monotonic()
            if delay > 0:
                time.sleep(delay)
        self.next_request_at = time.monotonic() + self.interval


class BatchStore:
    """Immutable Object Storage records for created instances and deletions."""

    def __init__(self, client, namespace, bucket, prefix):
        self.client = client
        self.namespace = namespace
        self.bucket = bucket
        self.prefix = prefix.strip("/")

    def _key(self, *parts):
        return "/".join([self.prefix, *parts]) if self.prefix else "/".join(parts)

    def save_instance(self, batch_id, run_id, index, record):
        self._put(self._key(batch_id, "runs", run_id, "instances", f"{index}.json"), record)

    def save_delete_request(self, batch_id, container_instance_id, record):
        self._put(
            self._key(batch_id, "delete-requests", f"{container_instance_id}.json"), record
        )

    def delete_requested_ids(self, batch_id):
        prefix = self._key(batch_id, "delete-requests") + "/"
        return {
            name.rsplit("/", 1)[-1][:-5]
            for name in self._list_names(prefix)
            if name.endswith(".json")
        }

    def instance_records(self, batch_id):
        prefix = self._key(batch_id, "runs") + "/"
        records = []
        for name in self._list_names(prefix):
            if "/instances/" not in name or not name.endswith(".json"):
                continue
            response = self.client.get_object(self.namespace, self.bucket, name)
            records.append(json.loads(response.data.content.decode("utf-8")))
        return records

    def _put(self, key, value):
        body = json.dumps(value, separators=(",", ":")).encode("utf-8")
        self.client.put_object(
            self.namespace, self.bucket, key, io.BytesIO(body), content_length=len(body)
        )

    def _list_names(self, prefix):
        names, start = [], None
        while True:
            response = self.client.list_objects(
                self.namespace, self.bucket, prefix=prefix, start=start, fields="name"
            )
            names.extend(item.name for item in response.data.objects)
            start = response.data.next_start_with
            if not start:
                return names


def handler(ctx, data: io.BytesIO = None):
    """OCI Functions entry point. Payload operation is either create or cleanup."""
    payload = _read_payload(data)
    batch_id = _required_id(payload, "batch_id")
    operation = payload.get("operation")
    _log_event("batch_invocation_started", batch_id=batch_id, operation=operation)
    try:
        store, container_client = _clients()
        if operation == "create":
            result = _create_batch(ctx, payload, batch_id, store, container_client)
        elif operation == "cleanup":
            result = _cleanup_batch(batch_id, store, container_client)
        else:
            raise ValueError("operation must be either 'create' or 'cleanup'")
    except Exception as exc:
        _log_event(
            "batch_invocation_failed",
            batch_id=batch_id,
            operation=operation,
            error=str(exc),
        )
        raise
    _log_event("batch_invocation_completed", batch_id=batch_id, operation=operation)
    return result


def _create_batch(ctx, payload, batch_id, store, container_client):
    instances = payload.get("instances")
    if not isinstance(instances, list) or not instances:
        raise ValueError("instances must be a non-empty array")

    run_id = _run_id(ctx, payload)
    max_retries = _max_retries(payload)
    create_requests_per_second, create_requests_per_minute = _create_request_rates()
    pacer = RequestPacer(create_requests_per_second, create_requests_per_minute)
    targets = _create_targets(instances)
    created, failures, pending = [], [], targets

    # Reconcile failed target slots until every requested resource is created or
    # the slot consumes its retry budget. A slot keeps the same retry token so a
    # retry after an ambiguous service failure remains idempotent.
    while pending:
        retry_pending = []
        for target in pending:
            target["attempts"] += 1
            spec = target["spec"]
            try:
                details = _create_details(spec, batch_id, run_id, target["target_index"])
                pacer.wait()
                response = container_client.create_container_instance(
                    details, opc_retry_token=f"{batch_id}-{run_id}-{target['slot_index']}"
                )
                record = {
                    "batch_id": batch_id,
                    "run_id": run_id,
                    "source_index": target["source_index"],
                    "target_index": target["target_index"],
                    "attempt": target["attempts"],
                    "container_instance_id": response.data.id,
                    "created_at": _now(),
                }
                # Persist immediately: cleanup can find instances even if a later item fails.
                store.save_instance(batch_id, run_id, target["slot_index"], record)
                created.append(record)
            except Exception as exc:  # Continue creating independent targets.
                target["last_error"] = str(exc)
                if target["attempts"] <= max_retries:
                    retry_pending.append(target)
                else:
                    failures.append(
                        {
                            "source_index": target["source_index"],
                            "target_index": target["target_index"],
                            "attempts": target["attempts"],
                            "error": target["last_error"],
                        }
                    )
        pending = retry_pending
    return _response(
        {
            "batch_id": batch_id,
            "run_id": run_id,
            "target_count": len(targets),
            "successful_count": len(created),
            "max_retries": max_retries,
            "create_requests_per_second": create_requests_per_second,
            "create_requests_per_minute": create_requests_per_minute,
            "created": created,
            "failures": failures,
        }
    )


def _cleanup_batch(batch_id, store, container_client):
    requested = store.delete_requested_ids(batch_id)
    delete_requests_per_second, delete_requests_per_minute = _delete_request_rates()
    pacer = RequestPacer(delete_requests_per_second, delete_requests_per_minute)
    deleted, failures = [], []
    for record in store.instance_records(batch_id):
        instance_id = record["container_instance_id"]
        if instance_id in requested:
            continue
        try:
            pacer.wait()
            container_client.delete_container_instance(instance_id)
            deletion = {"container_instance_id": instance_id, "delete_requested_at": _now()}
            store.save_delete_request(batch_id, instance_id, deletion)
            deleted.append(deletion)
        except oci.exceptions.ServiceError as exc:
            if exc.status == 404:  # An out-of-band delete is already clean.
                deletion = {"container_instance_id": instance_id, "delete_requested_at": _now(), "already_gone": True}
                store.save_delete_request(batch_id, instance_id, deletion)
                deleted.append(deletion)
            else:
                failures.append({"container_instance_id": instance_id, "error": str(exc)})
        except Exception as exc:
            failures.append({"container_instance_id": instance_id, "error": str(exc)})
    return _response(
        {
            "batch_id": batch_id,
            "delete_requests_per_second": delete_requests_per_second,
            "delete_requests_per_minute": delete_requests_per_minute,
            "delete_requested": deleted,
            "failures": failures,
        }
    )


def _create_details(spec, batch_id, run_id, target_index=1):
    for field in ("compartment_id", "availability_domain", "shape", "shape_config", "vnics", "containers"):
        if field not in spec:
            raise ValueError(f"instances[] is missing {field}")
    containers = [
        oci.container_instances.models.CreateContainerDetails(
            image_url=item["image_url"],
            display_name=item.get("display_name"),
            command=item.get("command"),
            arguments=item.get("arguments"),
            environment_variables=item.get("environment_variables"),
        )
        for item in spec["containers"]
    ]
    vnics = [
        oci.container_instances.models.CreateContainerVnicDetails(
            subnet_id=item["subnet_id"],
            display_name=item.get("display_name"),
            is_public_ip_assigned=item.get("is_public_ip_assigned"),
        )
        for item in spec["vnics"]
    ]
    tags = dict(spec.get("freeform_tags", {}))
    tags.update({"scheduled-batch-id": batch_id, "scheduled-batch-run-id": run_id})
    return oci.container_instances.models.CreateContainerInstanceDetails(
        compartment_id=spec["compartment_id"],
        availability_domain=spec["availability_domain"],
        shape=spec["shape"],
        shape_config=oci.container_instances.models.CreateContainerInstanceShapeConfigDetails(
            ocpus=spec["shape_config"]["ocpus"], memory_in_gbs=spec["shape_config"]["memory_in_gbs"]
        ),
        containers=containers,
        vnics=vnics,
        display_name=_indexed_display_name(spec.get("display_name"), target_index),
        freeform_tags=tags,
    )


def _clients():
    namespace = os.environ["OCI_OBJECT_STORAGE_NAMESPACE"]
    bucket = os.environ["OCI_BATCH_STATE_BUCKET"]
    signer = oci.auth.signers.get_resource_principals_signer()
    config = {}
    return (
        BatchStore(oci.object_storage.ObjectStorageClient(config, signer=signer), namespace, bucket, os.getenv("OCI_BATCH_STATE_PREFIX", "scheduled-container-batches")),
        oci.container_instances.ContainerInstanceClient(config, signer=signer),
    )


def _read_payload(data):
    raw = data.getvalue() if data else b"{}"
    try:
        return json.loads(raw.decode("utf-8"))
    except (AttributeError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise ValueError("request body must be a JSON object") from exc


def _max_retries(payload):
    value = payload.get("max_retries", DEFAULT_MAX_RETRIES)
    if isinstance(value, bool) or not isinstance(value, int) or value < 0:
        raise ValueError("max_retries must be a non-negative integer")
    return value


def _create_request_rates():
    return _request_rates(
        "OCI_BATCH_CREATE_REQUESTS_PER_SECOND",
        DEFAULT_CREATE_REQUESTS_PER_SECOND,
        "OCI_BATCH_CREATE_REQUESTS_PER_MINUTE",
        DEFAULT_CREATE_REQUESTS_PER_MINUTE,
    )


def _delete_request_rates():
    return _request_rates(
        "OCI_BATCH_DELETE_REQUESTS_PER_SECOND",
        DEFAULT_DELETE_REQUESTS_PER_SECOND,
        "OCI_BATCH_DELETE_REQUESTS_PER_MINUTE",
        DEFAULT_DELETE_REQUESTS_PER_MINUTE,
    )


def _request_rates(
    per_second_config_name,
    per_second_default,
    per_minute_config_name,
    per_minute_default,
):
    return (
        _positive_rate(per_second_config_name, per_second_default),
        _positive_rate(per_minute_config_name, per_minute_default),
    )


def _positive_rate(config_name, default):
    value = os.getenv(config_name, str(default))
    try:
        requests_per_second = float(value)
    except (TypeError, ValueError) as exc:
        raise ValueError(f"{config_name} must be a positive number") from exc
    if not math.isfinite(requests_per_second) or requests_per_second <= 0:
        raise ValueError(f"{config_name} must be a positive number")
    return requests_per_second


def _create_targets(instances):
    """Expand each specification into target slots for reconciliation."""
    targets = []
    for source_index, spec in enumerate(instances):
        if not isinstance(spec, dict):
            raise ValueError("instances[] entries must be objects")
        target_count = spec.get("target_count", 1)
        if isinstance(target_count, bool) or not isinstance(target_count, int) or target_count < 1:
            raise ValueError("instances[].target_count must be a positive integer")
        for target_index in range(1, target_count + 1):
            targets.append(
                {
                    "spec": spec,
                    "source_index": source_index,
                    "target_index": target_index,
                    "slot_index": len(targets),
                    "attempts": 0,
                }
            )
    return targets


def _indexed_display_name(display_name, target_index):
    if display_name is None:
        return None
    if not isinstance(display_name, str):
        raise ValueError("instances[].display_name must be a string")
    return display_name.replace("{index}", str(target_index))


def _required_id(payload, field):
    value = payload.get(field)
    if not isinstance(value, str) or not SAFE_ID.fullmatch(value):
        raise ValueError(f"{field} must contain only letters, numbers, '.', '_' or '-'")
    return value


def _run_id(ctx, payload):
    explicit = payload.get("run_id")
    if explicit:
        return _required_id(payload, "run_id")
    call_id = getattr(ctx, "CallID", lambda: None)()
    return call_id if isinstance(call_id, str) and SAFE_ID.fullmatch(call_id) else str(uuid.uuid4())


def _now():
    return datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")


def _log_event(event, **fields):
    print(json.dumps({"event": event, **fields}, sort_keys=True))


def _response(value):
    return json.dumps(value, sort_keys=True)
