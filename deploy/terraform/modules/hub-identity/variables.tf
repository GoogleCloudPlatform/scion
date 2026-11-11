variable "project_id" {
  description = "GCP project ID."
  type        = string
}

variable "project_number" {
  description = "GCP project number (shared.project_number from shared-lookup), used to build the IAM condition resource-name prefix for the hub SA's scoped secretmanager.admin grant."
  type        = string
}

variable "hub_name" {
  description = "Hub name. Service accounts are \"<hub_name>-hub\", \"<hub_name>-transport\", \"<hub_name>-agent\" (account_id max 30 chars, hence hub_name's 16-char max in its validation)."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{2,15}$", var.hub_name))
    error_message = "hub_name must match ^[a-z][a-z0-9-]{2,15}$."
  }
}

variable "agent_sa_project_roles" {
  description = "Project-level roles granted additively to the agent SA: with Workload Identity wired but no model-access role, an agent authenticating via the GCE metadata server (GCPIdentity.MetadataMode passthrough/assign, a hub-side setting — Terraform cannot grant it) still gets 403s calling Vertex. Default grants exactly the model-call role, nothing broader — this is the same project the agent SA's WI binding already reaches, so an *.admin/editor/owner role here would be as dangerous as the project-wide secretmanager grants this module deliberately doesn't give the agent SA (see the Agent SA comment above)."
  type        = list(string)
  default     = ["roles/aiplatform.user"]

  validation {
    # (?i)admin$ (case-insensitive), not \.admin$:
    # the literal-dot-then-"admin" form misses roles whose admin-ness is
    # mid-token, not a dotted suffix — roles/securityAdmin,
    # roles/resourcemanager.projectIamAdmin. Matching "admin" at the end of
    # the string case-insensitively catches both those and the *.Admin/
    # *.admin dotted forms in one pattern.
    #
    # The three iam.* roles are denied outright, not just admin-pattern
    # roles: each lets the agent SA impersonate the HUB SA (serviceAccountUser
    # signs as it, serviceAccountTokenCreator mints its tokens,
    # workloadIdentityUser lets a pod claim its WI binding) — an
    # authentication-scope escalation from "runs an agent" to "acts as the
    # hub", the same class of over-grant the Agent SA comment above already
    # rejects for Secret Manager.
    condition = alltrue([
      for r in var.agent_sa_project_roles :
      r != "roles/owner" && r != "roles/editor" &&
      !can(regex("(?i)admin$", r)) &&
      r != "roles/iam.serviceAccountTokenCreator" &&
      r != "roles/iam.serviceAccountUser" &&
      r != "roles/iam.workloadIdentityUser"
    ])
    error_message = "agent_sa_project_roles must not include roles/owner, roles/editor, any role ending in \"admin\" (case-insensitive), or roles/iam.serviceAccountTokenCreator, roles/iam.serviceAccountUser, roles/iam.workloadIdentityUser (each allows impersonating the hub SA)."
  }
}

variable "hub_workload_identity_ksa" {
  description = "Opt-in for a hub that runs as a Kubernetes pod (configurations/hub-gke) rather than as a Cloud Run service. When set, the hub GSA gets one roles/iam.workloadIdentityUser grant for exactly this { namespace, name } Kubernetes service account, so the pod's KSA can act as the hub GSA through Workload Identity. Default null creates nothing, which leaves every Cloud Run hub's plan unchanged. The grant is on the hub GSA resource only, never project-wide (agent_sa_project_roles above refuses a project-level workloadIdentityUser for the same reason)."
  type = object({
    namespace = string
    name      = string
  })
  default = null

  validation {
    # The same shape Kubernetes enforces for namespace and service account
    # names (RFC 1123 labels/subdomains, simplified to labels). Checked here
    # because a malformed value builds a member string IAM accepts but no
    # pod can ever match, which fails silently as a 403 at hub boot.
    condition = var.hub_workload_identity_ksa == null || (
      can(regex("^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$", var.hub_workload_identity_ksa.namespace)) &&
      can(regex("^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$", var.hub_workload_identity_ksa.name))
    )
    error_message = "hub_workload_identity_ksa.namespace and .name must be lowercase DNS labels (^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$)."
  }
}
