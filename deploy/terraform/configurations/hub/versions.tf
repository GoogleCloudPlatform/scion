terraform {
  # >= 1.9, not >= 1.6: the hub_name/state_prefix cross-variable validation
  # below needs 1.9's relaxed validation-block restrictions (referencing
  # another variable directly, not just resources visible after apply).
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
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 2.35"
    }
    tls = {
      source  = "hashicorp/tls"
      version = "~> 4.0"
    }
    # random and time are used transitively via hub-cloudrun and
    # cloudsql-database (both pin the same constraints in their own
    # versions.tf); declared explicitly here too so the root's
    # required_providers block reflects every provider the config actually
    # needs, matching what .terraform.lock.hcl already locks.
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
    time = {
      source  = "hashicorp/time"
      version = "~> 0.12"
    }
  }
}
