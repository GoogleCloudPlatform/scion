# release-images P2-FU3: the Homebrew publish fails on an image that is not anonymously pullable (ptone/scion#6625)

Date: 2026-11-07
Base: upstream main 17ed440
Developer: ri-fu3-dev

## Why

For v0.3.0-preview.6 the "Publish to Homebrew" images job passed, but
scion-grok-build, scion-muse-code and scion-pi answered 401 to anonymous
pulls. A package that a push creates starts private on ghcr, so the push
succeeds and the job stays green while Homebrew users cannot pull those
images.

## What changed

- `.github/workflows/publish-homebrew.yml`: the images job ends with a
  "Check images are anonymously pullable" step. It sources
  `image-build/scripts/lib/targets.sh` and captures `resolve_targets all`,
  the same expansion `build-images.sh --target all` pushes, so a harness
  added to the catalog is checked without editing the workflow (a failing
  catalog stops the step). For each image it asks ghcr for an anonymous pull
  token, then sends a HEAD request for
  `/v2/homebrew-scion/<image>/manifests/<tag>` with that token and an Accept
  list of the OCI index, docker manifest list, OCI manifest and docker
  manifest types. 200 passes. It reports a per-image `::error::` on 401 or
  403 (not anonymously pullable), 404 (tag not found, or no such package),
  any other code or curl failure, and a token outside
  `^[A-Za-z0-9._~+/=-]+$` (curl unescapes the quoted config string the token
  goes into). It checks every image, then exits 1 if any failed. The step
  has no secret and no GITHUB_TOKEN; curl runs with `-q` (no curlrc), with
  connect and total timeouts, and gets its headers on stdin (`-K -`), never
  as arguments; the token is never printed. The step has a 10 minute
  timeout. Because the formula job requires the images job to succeed, a
  failed check also stops the formula update.
- `docs/release-process.md`, Homebrew publishing: the two-line note on new
  packages starting private is replaced by a short paragraph saying the
  check fails on purpose for a new harness image, that an org admin makes
  the package public in the `homebrew-scion` organization's package settings
  and reruns the publish with images on (a full rebuild and re-push, after
  which the check runs again), and that a rerun with `publish_images=false`
  skips the check and publishes the formula with no pull check at all.

## Test

`image-build/scripts/test-build-images.sh` (run by `make test-image-scripts`)
extracts the step from the workflow, as the FU2 verify-step test does, and
runs it in the fixture checkout with a stub curl. The stub requires `-q`
first, rejects any other unexpected option, a second URL and a `-K` file
other than stdin, and logs each URL, the method, the timeouts and the `-K`
config. 75 checks:
- all images pullable passes and writes the summary;
- each catalog image is queried at exactly `/manifests/<tag>` with HEAD,
  including a harness added only to the fixture's catalog;
- every request has connect and total timeouts;
- the manifest config is the anonymous token and the Accept list only (OCI
  index and docker manifest list included), and the token request carries
  no credential;
- a manifest 401, 403 or 404, a token endpoint 401, a token-less 200, a
  token holding a backslash, a space, a dollar sign or a newline, a token
  cut to a trailing backslash by an escaped quote, and a curl failure on the
  token or the manifest request each fail with the per-image message, and
  the other images are still checked;
- an empty catalog fails with its message, and a failing catalog stops the
  step, both before any request;
- the token never appears in the output.

Mutation checks: dropping every `failed=1` fails 14 checks, hardcoding the
image list fails 8, dropping the token charset check fails 15, and dropping
`-q` fails 37.

## Review

Round 1 (ri-fu3-rev, sec-review-3) asked for the manifest HEAD in place of
tags/list (no tag-list pagination), the token charset check, `curl -q`, a
captured catalog, an exported REPO_ROOT, a stricter stub and the package
settings wording; all are in this commit.
Round 2 (ri-fu3-rev-2) approved; its follow-ups (the rerun guidance and
wrapping in the docs, the empty-catalog and token curl-failure tests, and a
truthful name for the escaped-quote case) are in this commit, with the
workflow unchanged.
Round 3 (ri-fu3-rev-3) asked for a shellcheck SC2016 directive on the
literal token bodies, the formula-without-check warning in the docs and a
rest-still-checked assertion for the manifest curl failure; all are in this
commit, with the workflow unchanged.

## Notes

- shellcheck and actionlint were not installed in the sandbox; the changed
  shell passed `bash -n`.
