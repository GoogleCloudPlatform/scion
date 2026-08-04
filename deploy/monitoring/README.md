# Cloud Monitoring Configuration

Alert policy definitions and uptime check configurations for Google Cloud
Monitoring. These YAML files document the monitoring rules for the Scion hosted
platform and can be applied using any of the supported provisioning tools.

## Applying the configuration

> **Important:** The YAML files in this directory use a custom DSL for
> readability — they do **not** conform to the raw GCP API schemas. You cannot
> pass them directly to `gcloud` commands. Use Terraform or Pulumi (below)
> which consume these files as configuration inputs, or manually translate
> each entry into a GCP-native AlertPolicy / UptimeCheckConfig / NotificationChannel
> JSON document before using the `gcloud` CLI.

### gcloud CLI (manual conversion required)

```bash
# The YAML files must be converted to GCP-native API format first.
# For each alert policy entry in alert-policies.yaml, create a separate
# GCP AlertPolicy JSON/YAML document, then apply:
#
#   gcloud alpha monitoring policies create --policy-from-file=<converted-policy>.json
#
# Notification channels and uptime checks require the same conversion:
#   gcloud beta monitoring channels create --channel-content-from-file=<converted-channel>.json
#   gcloud monitoring uptime create --config-from-file=<converted-check>.json
```

### Terraform

Use the `google_monitoring_alert_policy`, `google_monitoring_uptime_check_config`,
and `google_monitoring_notification_channel` resources. The YAML files in this
directory serve as the source of truth for threshold values, durations, and
display names.

### Pulumi

Use the `gcp.monitoring.AlertPolicy`, `gcp.monitoring.UptimeCheckConfig`, and
`gcp.monitoring.NotificationChannel` resources with values from these files.

## Prerequisites

1. Notification channels must be created before alert policies that reference
   them. See `notification-channels.yaml`.
2. Custom metrics (prefixed `custom.googleapis.com/scion.*`) must be emitted by
   the application before alert policies can evaluate. Metrics are emitted via
   the OTLP pipeline described in `.design/hosted/metrics-system.md`.
3. The GCP project must have the Cloud Monitoring API enabled.

## File inventory

| File | Contents |
|------|----------|
| `alert-policies.yaml` | 15 alert policies covering DB health, dispatch health, telemetry pipeline, and Hub auth |
| `uptime-checks.yaml` | 4 uptime checks for Hub and Broker health/readiness endpoints |
| `notification-channels.yaml` | 3 notification channel definitions (email, Slack, PagerDuty) |

## References

- [Cloud Monitoring alert policies](https://cloud.google.com/monitoring/alerts)
- [Cloud Monitoring uptime checks](https://cloud.google.com/monitoring/uptime-checks)
- [Cloud Monitoring notification channels](https://cloud.google.com/monitoring/support/notification-options)
- [Custom metrics](https://cloud.google.com/monitoring/custom-metrics)
- [Monitoring Query Language (MQL)](https://cloud.google.com/monitoring/mql)
