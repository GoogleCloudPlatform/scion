# Template cache clones authenticate through the scoped credential helper

**Date:** 2026-10-23
**Branch:** scion/tmplcache-scoped-helper-v2

## Change

Both host-side git steps of a remote template import from GitHub authenticate
the same way as the provisioning clone and the shared-workspace clone. The
steps are the branch resolution (`git ls-remote` in `resolveGitHubRef` /
`execGitLsRemote`) and the sparse checkout (`execSparseGitCheckout`), both in
`pkg/config/remote_templates.go`.

- The remote URL is the plain `https://github.com/<owner>/<repo>.git`.
- With an access token, each git command gets `util.ScopedCredentialHelperEnv`:
  - an empty `credential.helper` reset;
  - the constant helper scoped to `credential.https://github.com.helper`, which
    reads the credential from an environment variable at run time;
  - that variable, set in the git command's environment only.
- `git ls-remote` runs as host-side git: `util.HostGitArgs`, `util.HostGitEnv`,
  `GIT_TERMINAL_PROMPT=0`.
- The ls-remote seam `GitLsRemoteFunc` takes the token as its own argument
  (`func(ctx, repoURL, token string)`). The test stubs in
  `pkg/hub/internal/hubtestutil` and `cmd` follow that signature, and the pkg/hub
  stub records the token, so the pkg/hub import tests check that the project
  token reaches branch resolution.

## Credential guards (`pkg/util/git_host_token_guard_test.go`)

- `TestNoUnscopedHostGitCredentials` scans pkg/config along with the other
  host-git packages. Its exemptions are (file, pattern) pairs in
  `hostGitScanExemptions`, and there is one: `pkg/config/dsn_redact.go` for
  `url.UserPassword`, which writes a redaction marker over a database password.
  An exemption that matches nothing fails the test.
- `TestHostGitTokenFindings_PkgConfigSelfTest` checks the filter the guard runs:
  - a `url.UserPassword` call with a token in another pkg/config file is
    reported;
  - `dsn_redact.go` is exempt from `url.UserPassword` only, and its other
    patterns are reported.
- The whole-tree guard `TestNoEmbeddedGitCredentialWholeTree` has no exemption
  for `remote_templates.go`, and its dead-exemption check passes.

## Tests

- `TestSparseGitCheckout_TokenFromScopedCredentialHelper` uses the fake git from
  `installFakeGit`, which logs each call's `GIT_CONFIG_*` entries and the
  helper's credential variable. With a token, every call carries:
  - the reset and the github.com-scoped helper;
  - a helper value that names the variable and contains no secret value;
  - the token in the variable.

  Without a token, there is no credential entry and no variable. git's
  arguments and the error text contain no secret values.
- `TestResolveGitHubRef_TokenFromScopedCredentialHelper` and
  `TestResolveGitHubRef_NoTokenNoCredentialHelper` run branch resolution through
  the production ls-remote runner against a fake git. They check the exact argv
  (pins, `ls-remote --heads`, plain URL) and the same credential entries and
  variable.
- `TestExecGitLsRemote_Invocation` pins the host-side argv prefix.
- In pkg/hub, `assertLsRemoteCalledWithToken` expects the plain URL and the
  project token passed separately.
