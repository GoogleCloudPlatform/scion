terraform {
  # >= 1.9: hub_name's state_prefix validation references another variable
  # (same reason as configurations/hub).
  required_version = ">= 1.9"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 8.4"
    }
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 2.35"
    }
    helm = {
      source  = "hashicorp/helm"
      version = "~> 3.0"
    }
    # Used transitively by cloudsql-database (DB password) and hub-gke
    # (session secret); declared so this block lists every provider the
    # lock file locks.
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }
}
