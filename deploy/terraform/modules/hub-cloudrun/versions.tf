terraform {
  # >= 1.9, not >= 1.6: hub_write_timeout/broker_write_timeout's
  # cross-variable validation against var.timeout needs 1.9's relaxed
  # validation-block restrictions — the same reason configurations/hub's
  # root already requires >= 1.9 for its hub_name/state_prefix validation.
  # The calling root already satisfies this; this just makes the module's
  # own floor consistent with what it actually needs standalone.
  required_version = ">= 1.9"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 8.4"
    }
    # google-beta is required for google_cloud_run_v2_service.iap_enabled
    # (beta-only in every version checked, GA never has it) and for
    # google_iap_settings.access_settings.oauth_settings.client_id/
    # client_secret, which only exist from 8.0.0 onward (confirmed empty in
    # 6.50.0/7.0.0/7.20.0, present in 8.0.0/8.4.0) — hence pinning ~> 8.4
    # for the whole module set, not just this module.
    google-beta = {
      source  = "hashicorp/google-beta"
      version = "~> 8.4"
    }
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
