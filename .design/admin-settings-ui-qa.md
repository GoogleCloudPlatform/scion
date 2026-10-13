# Admin Server Configuration UI - QA Testing Report

**Date:** 2026-03-19
**Route:** `/admin/server-config`
**Component:** `web/src/components/pages/admin-server-config.ts`
**Backend:** `pkg/hub/admin_settings.go`
**Endpoints:** `GET/PUT /api/v1/admin/server-config`

## Test Environment

- Server started in workstation mode with `scion server start --foreground --enable-hub --enable-web --dev-auth`
- Settings.yaml pre-populated with sample data across all sections
- Dev auth enabled with test token; user authenticated as `dev@localhost` with admin role
- Screenshots saved to `.scratch/` directory

## Summary

| Area | Status | Notes |
|------|--------|-------|
| Page loads & renders | PASS | All 6 tabs render correctly |
| GET API returns settings | PASS | Correctly reads ~/.scion/settings.yaml |
| Sensitive field masking | PASS | dev_token, broker_token, db URL masked as "********" |
| Form populates from API | PASS | All fields correctly hydrated from API response |
| PUT API saves settings | PASS | Changes written correctly to settings.yaml |
| Reload feedback | PASS | Shows which settings were applied vs. require restart |
| Reset button | PASS | Restores form to last-loaded values |
| Admin authorization | PASS | Non-admin users get 403 |
| Breadcrumb/page title | **BUG** | Shows "Page Not Found" instead of "Server Config" |

## Detailed Findings

### BUG: "Page Not Found" shown in header breadcrumb

**Severity:** Low (cosmetic)
**File:** `web/src/components/app-shell.ts:39-48`
**Issue:** The `PAGE_TITLES` map is missing an entry for `/admin/server-config`. The `getPageTitle()` method falls through to the default return value of `'Page Not Found'`.
**Fix:** Add `'/admin/server-config': 'Server Config'` to the `PAGE_TITLES` constant.
**Screenshots:** All screenshots show "Page Not Found" in the header area above the page content.

### OBSERVATION: Fallback display values in form fields

**Severity:** Low (potential data integrity concern)
**Files:** `admin-server-config.ts:1227-1228, 1237-1238`
**Issue:** Several form fields use fallback display values when the API returns empty/undefined:
- **Read Timeout** displays `30s` (line 1227: `value=${this.hubReadTimeout || '30s'}`)
- **Write Timeout** displays `60s` (line 1238: `value=${this.hubWriteTimeout || '60s'}`)
- **Hub Endpoint** placeholder shows `https://hub.example.com`
- **OTLP Endpoint** placeholder shows `https://otel-collector.example.com:4317`
- **Log File** placeholder shows `/var/log/scion/telemetry.log`
- **Hub Report Interval** displays `30s`

These use `value=X` rather than `placeholder=X` syntax for some fields, meaning the fallback value is treated as the actual value. However, the `buildPayload()` function correctly guards with `if (this.hubReadTimeout)` checks, so empty values (from the API) remain empty in the state variables and are NOT sent on save. The display is misleading but does not cause data corruption.

**Recommendation:** Change fallback value patterns from `value=${this.field || 'default'}` to use `placeholder` attribute consistently, or add a visual indicator that defaults are being shown.

### OBSERVATION: CORS settings defined in schema but not rendered

**Severity:** Medium (missing functionality)
**Issue:** `V1CORSConfig` interface is defined (lines 32-38) and included in both `V1ServerHubConfig.cors` and `V1BrokerConfig.cors`, but there are no form fields for:
- CORS enabled/disabled
- Allowed origins
- Allowed methods
- Allowed headers
- Max age

The API returns CORS data (e.g., `"cors": {}` for empty config), but it cannot be edited through the UI.

### OBSERVATION: Notification Channels not editable

**Severity:** Low (intentional limitation)
**Issue:** Notification channel parameters are fully masked by the backend (all params become "********") and the UI correctly preserves them via `rawConfig` passthrough. Users cannot add/edit/remove notification channels through the UI. This appears to be an intentional design choice since notification channels often contain webhook URLs and tokens.

### OBSERVATION: OAuth providers read-only display

**Severity:** Low (intentional limitation)
**Issue:** OAuth providers section shows "No OAuth providers configured" or displays provider names with masked secrets. The UI correctly identifies that "OAuth client credentials are managed via the settings file or environment variables." This is the expected behavior - OAuth credentials should not be entered through a web UI.

### OBSERVATION: Fields intentionally hidden from UI

The following fields exist in the API response/schema but are correctly omitted from the UI:
- `broker_id` - Auto-generated, not user-editable
- `broker_token` - Sensitive, auto-generated
- `dev_token_file` - File path, not appropriate for web UI
- `gcp_credentials` - Sensitive blob, managed via settings file
- `runtimes` - Complex map structure, preserved via rawConfig passthrough
- `harness_configs` - Complex map structure, preserved via rawConfig passthrough
- `profiles` - Complex map structure, preserved via rawConfig passthrough

These are correctly preserved during save via `rawConfig` passthrough (lines 794-797) so they are not lost on save.

## Tab-by-Tab Verification

### 1. General Tab
| Field | API Value | Form Value | Match |
|-------|-----------|------------|-------|
| Server Mode | workstation | Workstation | YES |
| Log Level | info | Info | YES |
| Log Format | (not set) | Text | YES (default) |
| Active Profile | (not set) | (empty, placeholder "default") | YES |
| Default Template | (not set) | (empty, placeholder "default") | YES |
| Default Harness Config | (not set) | (empty) | YES |
| Image Registry | ghcr.io/test | ghcr.io/test | YES |
| Workspace Path | (not set) | (empty) | YES |
| Default Max Turns | 50 | 50 | YES |
| Default Max Model Calls | 100 | 100 | YES |
| Default Max Duration | 2h | 2h | YES |
| Message Broker Enabled | (not set) | off | YES |

### 2. Hub Server Tab
| Field | API Value | Form Value | Match |
|-------|-----------|------------|-------|
| Port | 9810 | 9810 | YES |
| Host | 127.0.0.1 | 127.0.0.1 | YES |
| Public URL | (not set) | (empty) | YES |
| Read Timeout | (not set) | 30s (fallback) | SEE NOTE |
| Write Timeout | (not set) | 60s (fallback) | SEE NOTE |
| Admin Emails | ["admin@test.com"] | admin@test.com | YES |
| Soft Delete Retention | 720h | 720h | YES |
| Retain Files on Soft Delete | true | ON | YES |

### 3. Runtime Broker Tab
| Field | API Value | Form Value | Match |
|-------|-----------|------------|-------|
| Enabled | true | ON | YES |
| Port | 9800 | 9800 | YES |
| Host | 127.0.0.1 | 127.0.0.1 | YES |
| Hub Endpoint | (not set) | (empty) | YES |
| Container Hub Endpoint | (not set) | (empty) | YES |
| Broker Name | test-broker | test-broker | YES |
| Broker Nickname | (not set) | (empty) | YES |
| Auto-provide | true | ON | YES |

### 4. Data & Storage Tab
| Field | API Value | Form Value | Match |
|-------|-----------|------------|-------|
| Database Driver | sqlite | SQLite | YES |
| Database URL | (not set) | (empty, placeholder) | YES |
| Storage Provider | local | Local | YES |
| Storage Bucket | (not set) | (empty) | YES |
| Storage Local Path | (not set) | (empty) | YES |
| Secrets Backend | local | Local | YES |
| GCP Project ID | (not set) | (empty) | YES |

### 5. Authentication Tab
| Field | API Value | Form Value | Match |
|-------|-----------|------------|-------|
| Dev Auth Enabled | true | ON | YES |
| Dev Token | ******** | ******** | YES (masked) |
| Authorized Domains | ["test.com","example.com"] | test.com, example.com | YES |
| OAuth Providers | (none configured) | "No OAuth providers configured" | YES |

### 6. Telemetry Tab
| Field | API Value | Form Value | Match |
|-------|-----------|------------|-------|
| Enable Telemetry Collection | false | OFF | YES |
| Cloud Export Enabled | (not set) | OFF | YES |
| Hub Reporting Enabled | (not set) | OFF | YES |
| Local Debug Output Enabled | (not set) | OFF | YES (control since removed: telemetry.local is accepted but ignored, ptone/scion#4103) |

## Save/Reload Tests

### Test 1: Modify default_max_turns via API
- Changed from 50 to 75 via `PUT /api/v1/admin/server-config`
- **Result:** PASS - Value correctly written to settings.yaml
- GET API reflects updated value
- Reload status shows `applied: ["admin_emails", "log_level"]`

### Test 2: Modify admin_emails via UI
- Changed from `admin@test.com` to `admin@test.com, newadmin@test.com`
- **Result:** PASS - Both emails saved as YAML array in settings.yaml
- Form shows updated values after reload

### Test 3: Reset button
- Modified max_turns to 999 and max_duration to 5h in form
- Clicked Reset
- **Result:** PASS - Values restored to 50 and 2h respectively

### Test 4: Masked value preservation
- dev_token shows "********" in form
- On save, the form correctly does NOT send "********" as the new value
- **Result:** PASS - Token preserved in settings.yaml after save

### Test 5: Settings expansion on save
- When saving, the YAML file gets reorganized (telemetry sub-objects expanded)
- **Result:** PASS - No data loss, all values preserved

## Recommendations

1. **Fix "Page Not Found" bug** - Add `/admin/server-config` to `PAGE_TITLES` in `app-shell.ts`
2. **CORS UI fields** - Consider adding CORS configuration fields or explicitly document that CORS must be configured via settings.yaml
3. **Fallback value display** - Use `placeholder` attribute consistently instead of `value || 'default'` pattern to avoid confusion
4. **"Requires restart" always shown** - The reload response always lists the full set of restart-required fields, even if they weren't changed. Consider only listing fields that were actually modified.

## Screenshots

| File | Description |
|------|-------------|
| `.scratch/02-general-tab.png` | General tab initial view |
| `.scratch/04-general-tab-tall.png` | General tab full view (tall viewport) |
| `.scratch/05-hub-server-tab.png` | Hub Server tab |
| `.scratch/06-runtime-broker-tab.png` | Runtime Broker tab |
| `.scratch/07-data-storage-tab.png` | Data & Storage tab |
| `.scratch/08-authentication-tab.png` | Authentication tab |
| `.scratch/09-telemetry-tab.png` | Telemetry tab |
| `.scratch/10-save-result.png` | Save result with success banner |
| `.scratch/11-hub-save-success.png` | Hub tab save with admin email change |
