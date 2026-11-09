# Hub: fixed client text for GitHub App and validate failures; upstream and error text rendered in logs

**Date:** 2026-11-09
**Branch:** hub-7498
**Issue:** ptone/scion#7498 (follow-ups to ptone/scion#7504)

## Behaviour

- GitHub App token mint errors (`githubapp.TokenMintError`) carry fixed text
  per error code in `Message`. GitHub's own text (its `message` field, an
  error body, or a transport error) is kept apart and read through
  `UpstreamDetail()`, for the hub to log once through `upstreamText`.
  GitHub API response bodies are read through a 4 KiB bounded reader
  (`upstreamBodyReadLimit`, kept equal to the hub's
  `upstreamErrorBodyReadLimit`).
- The agent GitHub token-refresh handler answers the fixed `Message` for a
  `TokenMintError` and a generic line for any other error. The project
  GitHub App status stores fixed text (`githubAppStatusMessage`). The error
  and GitHub's text reach the log once, in `mintGitHubAppToken`, rendered.
- The template and harness-config validate handlers answer
  `validation failed` and log the storage error through `upstreamText`.
- The Teams send-activity error and response body, and the GitHub App
  installation-repo list warnings, go through `upstreamText`.
- `apierr.WriteErrorFromErr` renders the 5xx log line through
  `principal.SanitizeForLog`, capped at 512 bytes (`apiLogDetailCap`, kept
  equal to `upstreamTextMaxBytes`). A nil error, which maps to the fallback
  500, logs as `<nil>` (`errLogDetail`).
- OAuth token responses: at every token decode site (`exchangeCodeForToken`,
  `exchangeGitHubCodeForTokenWithConfig`, `pollGoogleDeviceToken`,
  `pollGitHubDeviceToken`), a body that does not decode as JSON is rendered
  through `githubTokenErrorText` (`tokenDecodeError`). A form-encoded body
  carrying `access_token`, `refresh_token` or `id_token` (GitHub answers that
  way when it does not honour the Accept header) gets fixed text, and so
  does a JSON body that carries a credential but no error field: a
  credential never reaches an error or a log line.

## Tests

- `pkg/hub/apierr/errors_log_test.go`: the 5xx log line is rendered and
  capped; `renderLogDetail` slices before scanning; a nil error logs
  `error=<nil>` (`TestWriteErrorFromErr_NilErrorLogsNil`).
- `pkg/hub/githubapp/githubapp_upstream_text_test.go`: classified GitHub
  errors carry fixed text; non-JSON bodies; bounded body reads.
- `pkg/hub/channels_teams_upstream_text_test.go`,
  `pkg/hub/storage_validate_upstream_text_test.go`,
  `pkg/hub/oauth_upstream_text_test.go`: upstream text rendered in errors
  and logs; `TestOAuthTokenBodyCredentialNeverInError` covers the
  credential-bearing bodies at all four token decode sites.

## Verification

Run on the b02 broker through the K3 run kit (A0 overlay, tree sweep,
post-run process check): go vet of pkg/hub shard 6 (both tag sets); hub
shards 1, 2, 3, 4 and 6; the no_sqlite shards 2, 3 and 6; and go vet plus
go test of pkg/hub/githubapp and pkg/hub/apierr.

Two regressions were caught and fixed on the way: a nil error panicked in
`apierr.WriteErrorFromErr`'s 5xx log line (`TestWriteErrorFromErr_Mapping/
fallback/nil`, shard 2), and a 200 non-JSON GitHub token body was not
rendered (`TestOAuthGitHubNoAccessTokenNonJSONBodyRendered`, shard 6).

## Notes for later runs

- `go list -overlay` and the hub shards compile A0's overlay copy of each
  pkg/hub test file, not the repo file: after editing a test file, refresh
  its overlay copy, or the shard runs the stale copy.
- `hubshard -locate` on edited files can name a different shard than the
  overlay's build tag; the tag decides where the test runs.
