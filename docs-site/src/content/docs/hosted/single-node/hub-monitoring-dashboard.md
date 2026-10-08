---
title: Hub Monitoring Dashboard
description: Import the Cloud Monitoring dashboard for Hub database, dispatch, notification and launch reaper metrics.
---

The Hub exports operational metrics to Google Cloud Monitoring through OpenTelemetry. Scion includes a ready-made Cloud Monitoring dashboard for these metrics at [`deploy/monitoring/dashboards/scion-hub.json`](https://github.com/GoogleCloudPlatform/scion/blob/main/deploy/monitoring/dashboards/scion-hub.json). The dashboard shows each Hub replica as its own line, which makes it most useful in [HA hosted](/scion/hosted/ha/overview/) deployments with several replicas. It works the same way for a single-node Hub.

Use the dashboard for rates and per-instance history. The Hub's admin Health page shows only the current state of the instance that served the request.

## What the dashboard shows

| Section | Charts | Metrics |
| :--- | :--- | :--- |
| Database connection pool | Active, idle and max connections and waits per second, per pool (`store`, `events`) | `scion.db.pool.connections.*` |
| Broker dispatch | Claimed, done and failed per second; stuck pending messages; intent-to-done latency (p50, p95, p99) | `scion.dispatch.*` |
| Event notifications | Publish-to-deliver lag (p50, p95, p99); drops per second by reason | `scion.db.notify.*` |
| Launch reaper | Ticks per second by outcome; row errors per second; time disarmed | `scion.launch_reaper.*` |

Every chart is grouped by the `service_instance_id` label, so each Hub replica gets its own line. The dashboard also has a `scion_hub_id` filter at the top. All replicas of one Hub share a Hub ID, so this filter selects one Hub deployment when several Hubs export to the same project.

## Prerequisites

1. **Hub metrics export is on.** The Hub exports metrics when `server.hub.gcp_project_id` (`SCION_SERVER_HUB_GCPPROJECTID`) is set. The startup log then contains `Hub OTel metrics export enabled`. The metric groups can be turned off one by one with `SCION_METRICS_DB_POOL`, `SCION_METRICS_DB_NOTIFY` and `SCION_METRICS_DISPATCH`. A disabled group shows as empty charts.
2. **The Hub can write metrics.** The Hub's service account needs `roles/monitoring.metricWriter` on that project.
3. **You can create dashboards.** Importing needs `roles/monitoring.dashboardEditor` (or an equivalent role) on the project.

The notification charts need the Postgres database driver. A Hub on SQLite does not record the `scion.db.notify.*` metrics.

## Import the dashboard

The JSON file contains no project ID. Choose the project when you import it:

```bash
gcloud monitoring dashboards create \
  --project=PROJECT_ID \
  --config-from-file=deploy/monitoring/dashboards/scion-hub.json
```

Replace `PROJECT_ID` with the project in `server.hub.gcp_project_id`. The command prints the new dashboard's resource name (`projects/…/dashboards/DASHBOARD_ID`). Open it in the Google Cloud console under **Monitoring > Dashboards > Scion Hub**.

To find the dashboard later:

```bash
gcloud monitoring dashboards list --project=PROJECT_ID \
  --filter='displayName="Scion Hub"' --format='value(name)'
```

To pick up a newer version of the file, delete the old dashboard (`gcloud monitoring dashboards delete DASHBOARD_ID`) and create it again. You can also paste the file into the console's JSON editor for an existing dashboard. Charts update within one export interval (60 seconds) of new data arriving.

## How Hub metrics appear in Cloud Monitoring

The Hub uses the Google Cloud OpenTelemetry metric exporter. If you build your own charts or alert policies, use these mappings:

- **Metric type:** `workload.googleapis.com/` followed by the OpenTelemetry name, dots included. For example, `scion.dispatch.claimed` becomes `workload.googleapis.com/scion.dispatch.claimed`.
- **Replica label:** each Hub process sets the `service.instance.id` resource attribute to its own instance ID, a random UUID created at startup (`newInstanceID` in `pkg/hub/server.go`). It is prefixed with the pod name only when the `POD_NAME` environment variable is set. The UUID part is always there, so no two Hub processes share an instance ID: not replicas that share a Hub ID, and not a restarted container that keeps its pod name. Nothing in the configuration sets or overrides it. Hub traces (with `SCION_TRACING_ENABLED`) carry the same `service.instance.id`, so a replica's spans match its metric series. The Helm chart sets `POD_NAME` from the pod's name, so chart legends read as `<pod name>-<UUID>`; other deployments show bare UUIDs unless they set it. The instance ID becomes the metric label `service_instance_id`. You don't configure it. It changes every time a replica restarts, so each restart or rollout starts a new set of series and the old ones stop receiving points.
- **Deployment labels:** the `scion.hub.id` resource attribute becomes the metric label `scion_hub_id`. It comes from the Hub ID (`server.hub.hub_id`, environment variable `SCION_SERVER_HUB_HUBID`), which every replica of an HA Hub shares. Use it to filter by deployment, not to tell replicas apart. The `scion.hub.name` resource attribute becomes `scion_hub_name`. Don't group by it: when no name is configured it falls back to the host name, which is not a stable replica identity.
- **Label names:** the exporter replaces every character that is not a letter or digit with `_`. Point attributes such as `outcome` (reaper ticks), `reason` (notification drops) and `pool` (connection pools) are metric labels too.
- **Pool label:** the Hub has two connection pools, and every `scion.db.pool.*` point carries a `pool` label naming one: `store` (the main database pool) or `events` (the Postgres event pool). Group or filter pool charts by it so the two pools are not summed.
- **Monitored resource:** `generic_task`, with `job` set to `scion-hub` and `task_id` set to the replica's instance ID. `location` is `global` and `namespace` is empty.
- **Kinds:**

  | OpenTelemetry instrument | Metric kind | Value type | Typical aligner |
  | :--- | :--- | :--- | :--- |
  | Counter (for example `scion.dispatch.done`) | `CUMULATIVE` | `INT64` | `ALIGN_RATE` |
  | Gauge (for example `scion.db.pool.connections.active`) | `GAUGE` | `INT64` or `DOUBLE` | `ALIGN_MEAN` or `ALIGN_MAX` |
  | Histogram (for example `scion.dispatch.intent_to_done.duration`, in ms) | `CUMULATIVE` | `DISTRIBUTION` | `ALIGN_DELTA` with a percentile reducer |

:::caution[Known limits of the current metrics]
- `scion.db.pool.connections.wait_count` is a counter of the times a caller had to wait for a connection; neither database driver reports how many callers are waiting right now. The chart shows it as a rate. It replaces the `scion.db.pool.connections.waiting` gauge, which held the same running total as a gauge and is no longer exported.
- `scion.db.notify.subscriber.lag` counts notifications, not time: each delivery records how many notifications the event queues behind in the most-behind matching subscriber (0 when subscribers keep up, the buffer capacity when the event is dropped), and the hub samples the current depth per `scope` about every 30 seconds so the value returns to 0 when traffic stops. The dashboard shows notification lag as publish-to-deliver latency.
:::

## Link from the Health page

A link to this dashboard from the Hub's admin Health page is planned.

## Related guides

- [Metrics & OpenTelemetry](/scion/hosted/single-node/metrics/): agent and Hub telemetry configuration
- [Observability](/scion/hosted/single-node/observability/): logs, traces and troubleshooting
- [HA Overview](/scion/hosted/ha/overview/): running several Hub replicas
