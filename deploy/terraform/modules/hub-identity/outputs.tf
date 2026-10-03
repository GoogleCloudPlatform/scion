output "hub_sa_email" {
  description = "Hub (Cloud Run runtime) service account email."
  value       = google_service_account.hub.email
}

output "hub_sa_unique_id" {
  description = "Hub SA's numeric unique_id. Not secret. The hub's k8s client falls back to pkg/k8s/client.go's fallbackToGCEAuth (the kubeconfig names the gke-gcloud-auth-plugin exec, which the image doesn't have), and that fallback requests only the cloud-platform scope, not userinfo.email — so GKE identifies the caller by this numeric ID instead of the SA's email. agent-runtime-k8s's RoleBinding needs a second subject on this value or every hub API call to the cluster is denied as an unrecognized User."
  value       = google_service_account.hub.unique_id
}

output "transport_sa_email" {
  description = "Transport service account email (mints IAP OIDC tokens for agents)."
  value       = google_service_account.transport.email
}

output "agent_sa_email" {
  description = "Agent pod service account email (bound via Workload Identity in agent-runtime-k8s)."
  value       = google_service_account.agent.email
}

output "hub_iam_grants" {
  description = "All hub-SA IAM grant resources' .id values, bundled purely as a depends_on handle — hub-cloudrun's time_sleep.iam_propagation depends on this list, so a Cloud Run revision can't boot before these grants have had time to propagate. Deliberately .id (list(string)), not the whole resource objects: google_project_iam_member and google_service_account_iam_member have different attribute shapes (e.g. project vs. service_account_id), so a list(any) of the full objects fails type unification at the consuming module's typed variable boundary with \"all list elements must have the same type\" — reproduced credential-free with two different hashicorp/random resource types before fixing. .id still carries the same dependency edge as the full object would."
  value = [
    google_project_iam_member.hub_secretmanager_admin_hub_prefixed.id,
    google_project_iam_member.hub_cloudsql_client.id,
    google_project_iam_member.hub_cloudsql_instance_user.id,
    google_project_iam_member.hub_container_cluster_viewer.id,
    google_project_iam_member.hub_logging_log_writer.id,
    google_service_account_iam_member.hub_mints_transport_tokens.id,
    google_service_account_iam_member.hub_mints_own_tokens.id,
  ]
}

output "hub_iam_condition_expression_prefixed" {
  description = "The hub-prefixed secret-name prefix (ptone/scion#2152) used in the conditioned secretmanager.admin grant covering hub, user, and project scope alike, exposed only so hub-cloudrun's time_sleep can trigger a fresh wait if this expression ever changes (e.g. hub_name changes) — not meant for any other use."
  value       = local.hub_prefixed_secret_prefix
}
