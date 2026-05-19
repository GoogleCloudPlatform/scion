# Project Log: Rename Fixes v7

## Date: 2026-05-11

## Description
Implemented fixes for Code Review v7 issues related to the "grove" to "project" rename in versioned settings.

## Changes

### 1. Hub Project ID Remapping
Added logic to `LoadVersionedSettings` in `pkg/config/settings_v1.go` to remap `hub.project_id` to `hub.grove_id` in the Koanf configuration before unmarshaling. This ensures that the `SCION_HUB_PROJECT_ID` environment variable (which maps to `hub.project_id`) correctly populates the `ProjectID` field in `V1HubClientConfig` (which uses the `grove_id` Koanf tag for backward compatibility).

### 2. Snake Case Support in CLI Settings
Updated `UpdateVersionedSetting` and `GetVersionedSettingValue` in `pkg/config/settings_v1.go` to support `hub.project_id` and `hub.grove_id` keys. This allows users to use `scion config set hub.project_id <id>` or `scion config set hub.grove_id <id>` interchangeably with the existing camelCase keys.

### 3. V1 Settings Schema Update
Updated `pkg/config/schemas/settings-v1.schema.json` to promote `project_id` as the primary property name for identifying projects with the Hub. Added a note that `grove_id` is still accepted for backward compatibility. The primary environment variable for this field is now documented as `SCION_HUB_PROJECT_ID`.

## Verification Results

### Tests
- Created `pkg/config/v7_fixes_test.go` with specific test cases for remapping and snake_case keys.
- All tests in `pkg/config/` passed, including new and existing ones.
- `go test ./pkg/config/ -count=1` -> PASS

### Build
- `go build ./...` -> SUCCESS

## Observations
The remapping logic in `LoadVersionedSettings` is a clean way to handle the transition from `grove_id` to `project_id` at the configuration layer without needing to change all internal struct tags immediately, preserving backward compatibility with existing `settings.yaml` files.
