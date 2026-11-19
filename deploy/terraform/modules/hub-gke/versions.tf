terraform {
  # >= 1.9, not >= 1.6: enable_hub_pdb and enable_relay_pdb cross-variable
  # validations against var.hub_replica_count, var.conduit_relay_mode and
  # var.relay_replica_count need 1.9's relaxed validation-block restrictions
  # (same reason as modules/hub-cloudrun and configurations/hub-gke).
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
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
    tls = {
      source  = "hashicorp/tls"
      version = "~> 4.0"
    }
  }
}
