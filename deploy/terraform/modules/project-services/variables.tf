variable "project_id" {
  description = "GCP project ID to enable the required APIs in."
  type        = string
}

variable "services" {
  description = "List of service APIs to enable on the project."
  type        = list(string)
  default = [
    "run.googleapis.com",
    "sqladmin.googleapis.com",
    "secretmanager.googleapis.com",
    "container.googleapis.com",
    "artifactregistry.googleapis.com",
    "iap.googleapis.com",
    "iam.googleapis.com",
    "iamcredentials.googleapis.com",
    "compute.googleapis.com",
    "servicenetworking.googleapis.com",
    "file.googleapis.com",
    "cloudbuild.googleapis.com",
  ]
}
