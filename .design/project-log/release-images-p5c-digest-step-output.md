# release-images P5-c: Cloud Build push steps record their digest as step output (ptone/scion#6625)

Phase 5, part c: the yaml side only. No registry, Cloud Build or gcloud call was made. Tags that
already exist are unchanged and nothing is rebuilt: only trees cut from this change on record
digests.

## The reader (P5-a, unchanged)

`build-release-images.sh --channel-tag ... --builder cloud-build` takes each image digest from
`results.images` or from `results.buildStepOutputs`. A step output is base64, and each non-empty
decoded line must be exactly `<ref> sha256:<64 lowercase hex>` for a ref the run built:
`<registry>/<image>:<version>`, where the images are the `build-<image>` step ids of the submitted
config. A line for any other ref (such as `:<short-sha>`), a ref named twice, a line with a CR or
a leading space, or a digest that disagrees with `results.images` stops the run.

## What changed

- New `image-build/scripts/buildx-record-digest.sh <ref> buildx build <args...>`. It runs the
  build with `--metadata-file` and writes `<ref> <containerimage.digest>` to
  `$BUILDER_OUTPUT/output`. Before the build it refuses: no `--push`, `<ref>` not a `-t` value, a
  caller `--metadata-file`, or no `BUILDER_OUTPUT` directory. After the build it fails, writing
  nothing, when the metadata file is missing or empty, holds no digest or more than one digest, holds
  a digest that is not `sha256:<64 hex>`, or has an `image.name` that does not name `<ref>`. It needs
  bash, grep, mktemp and docker, all present in `gcr.io/cloud-builders/docker` (the cred-helper
  steps already run bash, grep and sed there).
- Every step that pushes `$_REGISTRY/<image>:$_TAG` runs through it (`entrypoint: 'bash'`, two
  leading args; the buildx args are unchanged), recording only the `:$_TAG` ref:
  `cloudbuild.yaml` (13 steps: push-core-base, scion-base, 10 harnesses, hub),
  `cloudbuild-common.yaml` (12), `cloudbuild-core-base.yaml` (push-core-base),
  `cloudbuild-scion-base.yaml` (1), `cloudbuild-harnesses.yaml` (10), `cloudbuild-hub.yaml` (1),
  `cloudbuild-thick.yaml` (13), and the generated single-harness config in
  `builders/cloud-build.sh`.
- Not changed: steps that push nothing (`build-core-base`, setup steps), so they write nothing;
  `cloudbuild-omni.yaml` (only `images:` pushes, and `results.images` carries that digest);
  `cloudbuild-hub-gke.yaml` (not a build-images.sh target, no `_TAG`); `harnesses/*/cloudbuild.yaml`
  (build-images.sh never submits them).
- Size: one line per step, at most about 355 bytes (a 38-character host, 30-character project,
  63-character repository, the longest image name and a 128-character tag). That is far under
  Cloud Build's per-step output limit (4 KB in older docs, 50 KB in newer ones).

## Tests

- `image-build/scripts/test-buildx-record-digest.sh` (in `make test-image-scripts`): stub docker,
  fixture metadata. Covers good pretty and compact JSON with exact bytes; missing, empty and
  malformed metadata; 63 or 65 hex and uppercase digests; two digest keys; a wrong or prefix-only
  `image.name`; a failed build; every refusal (docker not run, no output); a worst-case-length ref;
  and four images. A static check confirms each listed config records exactly its pushing
  `:$_TAG` steps, and that omni and hub-gke record nothing.
- `test-build-release-images.sh`: the four recorder outputs, base64 and `;`-joined, are fed to the
  real driver, which tags all four recorded digests.

## Follow-ups

- Operator note for `docs/release-process.md` (left to the P7/P8 owners of that file): a channel
  run with `--builder cloud-build` works only for release tags cut after this change; older tags
  stop at "no build-result digest".
- `harnesses/*/cloudbuild.yaml` could use the recorder too, for consistency (no reader today).
- Adjacent gap, not this change: `cloudbuild-omni.yaml`'s `build-*` ids put seven images that are
  never pushed into the driver's wanted list, so a `--target omni` channel run stops at "no
  build-result digest".
