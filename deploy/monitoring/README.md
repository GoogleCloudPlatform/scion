# Cloud Monitoring Configuration

Alert policy definitions and uptime check configurations for Google Cloud
Monitoring. These YAML files document the monitoring rules for the Scion hosted
platform and can be applied using any of the supported provisioning tools.

## Applying the configuration

### gcloud CLI

```bash
# Create notification channels first
gcloud beta monitoring channels create --channel-content-from-file=notification-channels.yaml

# Create alert policies (one per policy document)
gcloud alpha monitoring policies create --policy-from-file=alert-policies.yaml

# Create uptime checks
gcloud monitoring uptime create --config-from-file=uptime-checks.yaml
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
