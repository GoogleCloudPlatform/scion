# Template cache clones take credentials from the scoped helper environment

**Date:** 2026-10-23
**Branch:** scion/tmplcache-scoped-helper-v2

## Change

Both host-side git steps of a remote template import from GitHub now
authenticate the same way as the provisioning clone and the shared-workspace
clone. The steps are the branch resolution (`git ls-remote` in
`resolveGitHubRef` / `execGitLsRemote`) and the sparse checkout
(`execSparseGitCheckout`), both in `pkg/config/remote_templates.go`.

- The remote URL is always the plain `https://github.com/<owner>/<repo>.git`.
- When an access token is given, it is supplied through
  `util.ScopedCredentialHelperEnv`. That function resets `credential.helper`
  and adds a one-shot helper scoped to `credential.https://github.com.helper`
  in the environment of the git command.
- `git ls-remote` now runs as host-side git, like the sparse checkout:
  `util.HostGitArgs`, `util.HostGitEnv`, `GIT_TERMINAL_PROMPT=0`.
- The ls-remote seam `GitLsRemoteFunc` takes the token as its own argument
  (`func(ctx, repoURL, token string)`). The test stubs in
  `pkg/hub/internal/hubtestutil` and `cmd` follow the new signature. The pkg/hub
  stub records the token, so the pkg/hub import tests still check that the
  project token reaches branch resolution, now with the anonymous URL.

Everything else in the sparse checkout stays as it was: the same remote, sparse
pattern, host-side pins, skipped LFS smudge and disabled prompts.

## Tests

- `TestSparseGitCheckout_TokenFromScopedCredentialHelper` uses the fake git from
  `installFakeGit`, which now also logs each call's `GIT_CONFIG_*` entries. It
  asserts that:
  - the remote URL is the anonymous URL;
  - the token is on no argv and in no error text;
  - with a token, every call carries exactly the empty `credential.helper` reset
    plus the helper scoped to `https://github.com`;
  - without a token, no `credential.*` entry is present.
- `TestResolveGitHubRef_TokenFromScopedCredentialHelper` and
  `TestResolveGitHubRef_NoTokenNoCredentialHelper` run branch resolution through
  the production ls-remote runner against a fake git. They check the exact argv
  (pins, `ls-remote --heads`, anonymous URL), that the token is on no argv and
  in no URL, and that the credential entries are the same as above.
- `TestExecGitLsRemote_Invocation` pins the host-side argv prefix.
- In pkg/hub, `assertLsRemoteCalledWithToken` expects the anonymous URL and the
  project token passed separately.

## Notes

- This branch is stacked on the branch that introduces
  `util.ScopedCredentialHelperEnv` and the host-side git pins. Both touch
  `execSparseGitCheckout` and the `installFakeGit` test helper, so they land
  in that order.
- `TestVersionedSettings_TagsAgree` fails at the base commit of this branch
  for reasons unrelated to this change. It passes on current `main`.
