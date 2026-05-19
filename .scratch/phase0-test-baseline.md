# Phase 0 Test Baseline

Captured on: 2026-05-09

## Build Summary

- `go build ./...`: **PASSED**

## Test Summary

- `go test ./...`: **FAILED**

### Package Failures

| Package | Result |
|---------|--------|
| `github.com/GoogleCloudPlatform/scion/cmd` | FAIL |
| `github.com/GoogleCloudPlatform/scion/cmd/sciontool/commands` | FAIL |
| `github.com/GoogleCloudPlatform/scion/pkg/agent` | FAIL |
| `github.com/GoogleCloudPlatform/scion/pkg/config` | FAIL |
| `github.com/GoogleCloudPlatform/scion/pkg/harness` | FAIL |
| `github.com/GoogleCloudPlatform/scion/pkg/hub` | FAIL |
| `github.com/GoogleCloudPlatform/scion/pkg/hubsync` | FAIL |
| `github.com/GoogleCloudPlatform/scion/pkg/store/sqlite` | FAIL |

### Individual Test Failures (Representative Samples)

- **cmd**:
    - `TestDeleteStopped_RequiresGroveContext`: `docker ps failed: exec: "docker": executable file not found in $PATH`
- **pkg/agent**:
    - `TestSettingsTelemetryMergedIntoStart`
- **pkg/config**:
    - `TestIsInsideGrove`
    - `TestLoadVersionedSettings_TelemetryHierarchyMerge`
    - `TestLoadVersionedSettings_TelemetryEnvOverride`
- **pkg/harness**:
    - `TestGitCloneWorkspace_DefaultEnvValues`
    - `TestGitCloneWorkspace_NonZeroUIDChownsWorkspace`
- **pkg/hub**:
    - `TestMessageBrokerProxy_UserMessageDelivery`
    - `TestMessageBrokerProxy_EnsureGroveSubscriptionsIncludesUserMessages`
    - `TestMessageBrokerProxy_StartBootstrapsExistingGroves`
    - `TestMessageBrokerProxy_GroveSubscriptionDedup`
- **pkg/hubsync**:
    - `TestEnsureHubReady_GlobalFallbackWithHubEnabled`
    - `TestEnsureHubReady_GlobalFallbackWithHubDisabled`
- **pkg/store/sqlite**:
    - `TestMaintenanceOperationsSeeded`: `should have 4 item(s), but has 5`

## Observations

- The failure in `TestDeleteStopped_RequiresGroveContext` appears to be due to `docker` not being available in the test environment.
- Many failures seem related to Telemetry settings and Hub synchronization.
- `pkg/store/sqlite` has a mismatch in the number of seeded maintenance operations (expected 4, found 5).

## Pre-rename Baseline

- **Initial Grove Count**: 22194
