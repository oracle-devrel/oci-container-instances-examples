# Terraform Deployment

This directory contains the Terraform for the repository's OCI deployment.

It provisions:

- networking for the container instance
- an optional OCI log group and custom log for the log forwarder
- IAM resources for resource principal access to OCI Logging and OCI Monitoring
- the container instance running the generator container, with optional log forwarder and metrics forwarder sidecars

## What Gets Created

Terraform creates:

- `oci_core_vcn`
- `oci_core_internet_gateway`
- `oci_core_route_table`
- `oci_core_security_list`
- `oci_core_subnet`
- `oci_logging_log_group`
- `oci_logging_log`
- `oci_identity_dynamic_group`
- `oci_identity_policy`
- `time_sleep`
- `oci_container_instances_container_instance`

The `time_sleep` resource adds the configured wait before the container instance is created.

## Prerequisites

- Terraform `>= 1.5.0`
- OCI credentials available to the Terraform OCI provider
- the generator image already pushed to a registry reachable by the container instance
- if enabled, the log forwarder image already pushed to a registry reachable by the container instance
- optionally, a metrics forwarder image already pushed to a registry reachable by the container instance

## Configure Variables

Create a local tfvars file:

```bash
cp terraform.tfvars.example terraform.tfvars
```

Then set at least:

- `tenancy_ocid`
- `compartment_id`
- `region`
- `availability_domain`
- `generator_image_url`

Optional but commonly adjusted:

- `enable_log_forwarder`
- `log_forwarder_image_url`
- `enable_metrics_forwarder`
- `metrics_forwarder_image_url`
- `metric_file_path`
- `metrics_namespace`
- `generator_ingress_cidrs`
- `display_name`
- `log_group_display_name`
- `custom_log_display_name`
- `log_forwarder_logrotate_enabled`
- log forwarder flush, chunk, and rotation settings
- metrics forwarder flush, chunk, and rotation settings

## OCI Auth for Terraform

Terraform itself needs OCI credentials before `plan` or `apply`.

Typical local setup uses either:

- your default OCI CLI config in `~/.oci/config`
- standard OCI provider environment variables such as:
  `OCI_TENANCY_OCID`, `OCI_USER_OCID`, `OCI_FINGERPRINT`, `OCI_PRIVATE_KEY_PATH`, and `OCI_REGION`

You can also export variable values directly for Terraform, for example:

```bash
export TF_VAR_tenancy_ocid='ocid1.tenancy.oc1..example'
export TF_VAR_compartment_id='ocid1.compartment.oc1..example'
export TF_VAR_region='us-ashburn-1'
```

## Apply

```bash
cd samples/oci-logging-monitoring-sidecars/container_instance
terraform init
terraform plan -out tfplan
terraform apply tfplan
```

## Outputs

Useful outputs include:

- `container_instance_id`
- `container_instance_state`
- `vcn_id`
- `subnet_id`
- `log_group_id`
- `custom_log_id`
- `log_forwarder_enabled`
- `metrics_forwarder_enabled`

Show them later with:

```bash
terraform output
```

## Update

```bash
terraform plan -out tfplan
terraform apply tfplan
```

## Destroy

```bash
terraform destroy
```

## Notes

- The log forwarder container is resource-principal-only when enabled.
- The optional metrics forwarder container is also resource-principal-only.
- The log forwarder mounts a dedicated `EMPTYDIR` volume at `/mnt/log-forwarder-status` for its spool and checkpoint data.
- The optional metrics forwarder mounts a dedicated `EMPTYDIR` volume at `/mnt/metrics-forwarder-status` for its spool and checkpoint data.
- When enabled, the custom log OCID is created by Terraform and injected into the log forwarder container automatically.
- OCI Monitoring does not require a pre-created metric resource, but the IAM policy must allow `use metrics` in the target compartment.
