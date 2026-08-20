# Key Terraform outputs for integrating with the created runtime and optional
# OCI Logging resources after `terraform apply`.

output "container_instance_id" {
  description = "OCID of the created container instance."
  value       = oci_container_instances_container_instance.logging_test.id
}

output "container_instance_state" {
  description = "Lifecycle state of the container instance."
  value       = oci_container_instances_container_instance.logging_test.state
}

output "subnet_id" {
  description = "OCID of the created subnet."
  value       = oci_core_subnet.logging_test.id
}

output "vcn_id" {
  description = "OCID of the created VCN."
  value       = oci_core_vcn.logging_test.id
}

output "dynamic_group_name" {
  description = "Name of the runtime dynamic group."
  value       = oci_identity_dynamic_group.log_forwarder_runtime.name
}

output "policy_name" {
  description = "Name of the runtime IAM policy."
  value       = oci_identity_policy.log_forwarder_runtime.name
}

output "log_group_id" {
  description = "OCID of the OCI log group."
  value       = local.log_forwarder_enabled ? oci_logging_log_group.log_forwarder[0].id : null
}

output "custom_log_id" {
  description = "OCID of the OCI custom log used by the log forwarder."
  value       = local.log_forwarder_enabled ? oci_logging_log.log_forwarder[0].id : null
}

output "log_forwarder_enabled" {
  description = "Whether the log forwarder sidecar is enabled in this deployment."
  value       = local.log_forwarder_enabled
}

output "metrics_forwarder_enabled" {
  description = "Whether the metrics forwarder sidecar is enabled in this deployment."
  value       = local.metrics_forwarder_enabled
}
