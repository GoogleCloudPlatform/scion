# Phase 3: IAP Proxy Integration

**Date:** 2026-09-15
**Issue:** ptone/scion#1575
**Branch:** scion/sn-vm-iap-proxy (based on scion/sn-vm-deploy-script)

## Summary

Extended `scripts/single-node-vm/deploy.sh` with two new phases (Phase 4 and
Phase 5) that deploy a Cloud Run IAP reverse proxy, enable IAP authentication,
and reconfigure the hub for proxy auth mode.

## Changes

### deploy.sh
- Updated header comments to reflect the full deploy flow with IAP
- Added `cloudbuild.googleapis.com` and `artifactregistry.googleapis.com` to
  the API enable list (required for `gcloud run deploy --source`)
- Phase 3 now writes settings.yaml via heredoc (not template) with dev auth
  for initial startup
- **Phase 4: IAP Proxy** (new):
  1. Gets VM internal IP via `gcloud compute instances describe`
  2. Deploys Cloud Run service from `extras/cloudrun-iap-proxy` source with
     Direct VPC Egress (`--network=default --subnet=default --vpc-egress=all-traffic`)
  3. Sets `TARGET_URL=http://VM_IP:8080` to route traffic to the hub
  4. Enables IAP on the Cloud Run service
  5. Binds `roles/iap.httpsResourceAccessUser` for the deploying operator
  6. Waits 60 seconds for IAP enforcement to activate
- **Phase 5: Finalize** (new):
  1. Computes IAP audience using format `/projects/NUMBER/locations/REGION/services/SERVICE`
     (matches `di_build_iap_audience` in `scripts/single-node/deploy.sh`)
  2. Overwrites settings.yaml with proxy auth configuration (mode: proxy, provider: iap)
  3. Restarts scion-hub.service to pick up the new auth config
  4. Runs a post-restart health check
  5. Prints the Cloud Run service URL as the access URL

### settings.yaml.tpl
- Updated to document the final (proxy auth) configuration as reference
- deploy.sh generates settings.yaml via heredoc, so the template is documentation
  rather than a sed template

## Design Decisions

1. **IAP audience format**: Used `/projects/PROJECT_NUMBER/locations/REGION/services/SERVICE_NAME`,
   matching the pattern in `scripts/single-node/deploy.sh` (`di_build_iap_audience`).
   This is the standard format for Cloud Run services with IAP.

2. **Two-stage settings.yaml**: Phase 3 writes dev-mode settings for the initial
   health check, Phase 5 overwrites with proxy auth. This ensures the hub starts
   successfully before IAP is configured.

3. **--allow-unauthenticated on Cloud Run**: Correct because IAP handles
   authentication at the load balancer level, not Cloud Run IAM.

4. **Agent access**: Agents running on the VM connect via localhost:8080 (no IAP
   needed). This is documented in the final deploy output.
