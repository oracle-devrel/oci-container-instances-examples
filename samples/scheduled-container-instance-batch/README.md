# Scheduled Container Instance Batches

This OCI Function creates and cleans up a batch of OCI Container Instances on
an OCI Resource Scheduler schedule. It supports both a one-time schedule and a
recurring cron schedule. Every successfully created instance is written to an
OCI Object Storage state bucket with its OCID. A separate cleanup invocation
uses those records to delete the instances, including instances created before
an earlier partial failure.

OCI Resource Scheduler invokes scheduled Functions in detached mode, so no
function process waits for a start or termination time.

## Architecture

```text
Resource Scheduler -- create payload --> OCI Function --> Container Instances
                                        |                     |
                                        +--> Object Storage <-+ (OCIDs)

Resource Scheduler -- cleanup payload -> OCI Function --> delete Container Instances
```

The create and cleanup schedules must use the same `batch_id`. Cleanup is
idempotent: accepted deletes (and instances already deleted elsewhere) are
recorded, so a retry only processes remaining instances.

## Prerequisites

- OCI CLI and Fn Project CLI, configured for the target region.
- An OCI Functions application in the same region.
- A pre-created Object Storage bucket for state records. Keep the bucket
  private and apply lifecycle retention appropriate to your audit needs.
- Network, image, and Container Instance prerequisites described in the
  repository [getting-started guide](../../GETTINGSTARTED.md).

## Configure and deploy

Set the two required function configuration values. The namespace is the
Object Storage namespace (not the tenancy OCID).

```bash
cd samples/scheduled-container-instance-batch
fn deploy --app <function-app-name>
fn config function <function-app-name> scheduled-container-instance-batch \
  OCI_OBJECT_STORAGE_NAMESPACE '<object-storage-namespace>'
fn config function <function-app-name> scheduled-container-instance-batch \
  OCI_BATCH_STATE_BUCKET '<state-bucket-name>'
```

Optionally set `OCI_BATCH_STATE_PREFIX` to isolate state within a shared
bucket. The default is `scheduled-container-batches`.

Create calls, including retries, and cleanup delete calls are rate-limited to
1 requests per second (RPS) and 10 requests per minute (RPM) by default. Both
limits are enforced, so the stricter limit determines the steady request pace.
Set positive decimal rates independently for create and cleanup operations:

```bash
fn config function <function-app-name> scheduled-container-instance-batch \
  OCI_BATCH_CREATE_REQUESTS_PER_SECOND 1
fn config function <function-app-name> scheduled-container-instance-batch \
  OCI_BATCH_CREATE_REQUESTS_PER_MINUTE 10
fn config function <function-app-name> scheduled-container-instance-batch \
  OCI_BATCH_DELETE_REQUESTS_PER_SECOND 1
fn config function <function-app-name> scheduled-container-instance-batch \
  OCI_BATCH_DELETE_REQUESTS_PER_MINUTE 10
```

Use [payload.create.example.json](payload.create.example.json) as the create
payload and [payload.cleanup.example.json](payload.cleanup.example.json) as
the cleanup payload. Replace every example OCID and instance setting. Each
entry in `instances` creates one Container Instance by default. To create a
number of identical instances, set `target_count` on one entry instead of
duplicating it. When `display_name` contains `{index}`, the function replaces
it with the one-based target index (`batch-{index}` becomes `batch-1`,
`batch-2`, and so on).

Create requests reconcile each target slot until it succeeds or exhausts the
retry budget. Set the top-level `max_retries` to control retries after the
initial request; it defaults to `3` (at most four create calls per target).
The response reports the requested `target_count`, `successful_count`, and any
targets that still failed after their retry budget.

The supported input fields are intentionally limited to the core Container
Instance settings: compartment, AD, shape and flex shape configuration, VNICs,
containers, display name, and freeform tags. The function adds the
`scheduled-batch-id` and `scheduled-batch-run-id` tags automatically.

## Grant permissions

Create a dynamic group containing the deployed Function, then grant it access
to Container Instances and its state bucket's compartment. Substitute your
OCIDs and group name:

```
ALL {resource.type = 'fnfunc', resource.id = '<function-ocid>'}

Allow dynamic-group scheduled-batch-function to manage compute-container-family in compartment <target-compartment>
Allow dynamic-group scheduled-batch-function to manage object-family in compartment <state-bucket-compartment>
Allow dynamic-group scheduled-batch-function to use virtual-network-family in compartment <target-compartment>
```

If images are in OCIR or another private registry, grant the additional
repository or Vault permissions required by that image configuration.

## Schedule a one-time batch

In the OCI Console, open **Developer Services → Functions → your function →
Schedules**, then choose **Add Schedule**. Create a schedule with the **One
time** interval, set the UTC start date and time, and add the complete create
payload from `payload.create.example.json` as the invocation payload.

Create a second one-time schedule for the required UTC termination time and
add `payload.cleanup.example.json`. This is the explicit termination-time
option: the cleanup schedule can be any future time independent of the create
time.

## Schedule a recurring batch

Create two resource schedules in the same way, choosing **Cron expression**:

- Create: for example, `0 8 * * mon-fri` to create the batch at 08:00 UTC on
  weekdays, with the create payload.
- Cleanup: for example, `0 18 * * mon-fri` to request deletion at 18:00 UTC
  on weekdays, with the cleanup payload.

Use a unique `batch_id` per independent lifecycle. Ensure the cleanup cadence
does not overlap the next create run, because cleanup intentionally removes all
not-yet-cleaned instances belonging to that batch ID.

For each Resource Scheduler schedule, create a dynamic group using that
schedule OCID and let it invoke the function:

```
ALL {resource.type='resourceschedule', resource.id='<resource-schedule-ocid>'}

Allow dynamic-group scheduled-batch-create-schedule to manage functions-family in compartment <function-compartment>
```

Repeat the dynamic group and policy for the cleanup schedule. The person who
creates schedules also needs permission to manage `resource-schedule-family`.

## Manual invocation for a smoke test

The Resource Scheduler invokes this function in detached mode. Use detached
mode for a manual smoke test as well, particularly for batches that take longer
than the caller's synchronous timeout (commonly 60 seconds). A 444
`StatusConnectionClosedWithoutResponse` means that caller disconnected while
the function was still running; the function cannot catch or return a response
over that closed connection.

```bash
fn invoke detached <function-app-name> scheduled-container-instance-batch < payload.create.example.json
fn invoke detached <function-app-name> scheduled-container-instance-batch < payload.cleanup.example.json
```

Detached invocation returns as soon as OCI Functions accepts the work. Inspect
the function logs for `batch_invocation_started`, `batch_invocation_completed`,
or `batch_invocation_failed`, and inspect the state bucket under
`scheduled-container-batches/<batch_id>/` to retain an audit trail of create
and delete requests.

## Operational notes

- A cleanup response means OCI accepted deletion requests; it does not wait for
  asynchronous deletion work requests to finish.
- Failed deletion requests remain unmarked and are retried the next time the
  cleanup schedule runs.
- Do not reuse a batch ID for unrelated resources. If a batch is intentionally
  retired, retain or archive its state prefix before reusing the ID.
