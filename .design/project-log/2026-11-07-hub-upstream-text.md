# Hub: upstream response text in error and log lines; fixed client text for object-store failures

**Date:** 2026-11-07
**Branch:** hub-upstream-text
**Issue:** ptone/scion#7504

## Behaviour

- Text received from an upstream service (OAuth token, userinfo, device-flow
  and OIDC userinfo responses; the Teams token endpoint; Google credential
  validator decode errors) reaches error values and server log lines only
  through `upstreamText`: capped at 512 bytes, with control, format and
  line-separator runes and invalid UTF-8 replaced by U+FFFD.
- `upstreamText` (`pkg/hub/upstream_text.go`) is a fixed-cap wrapper around
  the existing `sanitizeForLog` (`principal.SanitizeForLog`). It sanitizes a
  bounded prefix of its input, so a large body is not scanned in full.
  Upstream error bodies are read through a bounded reader.
- OAuth errors name the HTTP status code. When the GitHub code exchange
  returns no access token, the error names GitHub's `error` and
  `error_description` fields, rendered through `upstreamText`; the rest of
  the body is not rendered.
- Template, harness-config, skill and workspace upload, upload-URL, verify
  and download-URL handlers, and agent create's workspace upload-URL step,
  answer object-store failures with fixed text and their existing status
  (502) and error code (`runtime_error`). The object-store detail goes to
  the server log once, through `upstreamText`. Agent create records the
  fixed cause `errWorkspaceUploadURLs` in its rollback audit record.

## Follow-ups (out of scope here)

- `apierr.WriteErrorFromErr` logs the 5xx error for template and
  harness-config commit failures; the client text there is fixed. Capping
  that log line belongs to the shared apierr package.
- The Teams send-activity error (`channels_teams.go`, `Deliver`) carries up
  to 1 KiB of the response body into a log line.
- The `ValidateStorage` handlers (`template_handlers.go`,
  `harness_config_handlers.go`) relay storage errors in their 502 answer.
- `writeRootTempFile` and the template cache's `copyFile` and index write
  take their file modes from the process umask.

## Tests

- `TestUpstreamText`, `TestUpstreamText_MarkerBody` (helper table test).
- `TestOAuthUpstreamErrorBodyRendered`, `TestOAuthDeviceTokenErrorFieldRendered`,
  `TestOAuthGitHubNoAccessTokenNamesError`, `TestOAuthGitHubExchangeSuccess`,
  `TestCLIDeviceAuthorize_UpstreamBodyLoggedNotReturned`. The device-authorize
  test configures the device-flow GitHub provider with a client ID and a
  client secret, so the request reaches the device-code call it checks.
- `TestTeamsTokenEndpointErrorBodyRendered`.
- `TestGoogleValidatorDecodeErrorRendered`, `TestGoogleValidatorTokenInfoErrorFieldRendered`.
- `object_store_upstream_text_test.go`: one test per object-store site
  (template, harness-config and skill upload and download, skill multipart
  publish, workspace sync-to, sync-to finalize and sync-from, agent create).
- The pkg/hub shard runs assert file modes of 0644 and 0755, so they run
  under the usual umask 022.
