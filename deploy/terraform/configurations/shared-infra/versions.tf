terraform {
  # Matches configurations/hub's floor (needed there for cross-variable
  # validation); kept in sync rather than split per-root.
  required_version = ">= 1.9"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 8.4"
    }
    google-beta = {
      source  = "hashicorp/google-beta"
      version = "~> 8.4"
    }
  }
}
