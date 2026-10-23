# Template cache clones take credentials from the scoped helper environment

**Date:** 2026-10-23
**Branch:** scion/tmplcache-scoped-helper-v2

## Change

The template cache's sparse checkout (`execSparseGitCheckout` in
`pkg/config/remote_templates.go`) now authenticates the same way as the
provisioning clone and the shared-workspace clone: the remote URL is always
the plain `https://github.com/<owner>/<repo>.git`, and when an access token is
given it is supplied through `util.ScopedCredentialHelperEnv`. That function
resets `credential.helper` and adds a one-shot helper scoped to
`credential.https://github.com.helper`, in the environment of every git
command in the checkout.

Everything else stays as it was: the same remote, sparse pattern, host-side
pins (`util.HostGitArgs`), skipped LFS smudge and disabled terminal prompts.

## Tests

`TestSparseGitCheckout_TokenFromScopedCredentialHelper` (replaces
`TestSparseGitCheckout_AuthTokenInURL`) extends the fake git used by
`installFakeGit` to log each call's `GIT_CONFIG_*` entries. It asserts that:

- the remote URL is the anonymous URL, with or without a token;
- the token is on no git argv and in no error text;
- with a token, every call carries exactly the empty `credential.helper`
  reset plus the helper scoped to `https://github.com`;
- without a token, no `credential.*` entry is present.

## Notes

- This branch is stacked on the branch that introduces
  `util.ScopedCredentialHelperEnv` and the host-side git pins. Both touch
  `execSparseGitCheckout` and the `installFakeGit` test helper, so they land
  in that order.
- `TestVersionedSettings_TagsAgree` fails at the base commit of this branch
  for reasons unrelated to this change. It passes on current `main`.
