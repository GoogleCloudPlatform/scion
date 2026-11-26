# `agydecode` Golden Fixtures

These golden JSON files pin the canonical output of `pkg/trajectory/agydecode`
against synthesized, scrubbed SQLite conversation databases built in
`fixture_test.go` (containing no real conversation text, paths, or secrets).

## Verified `agy` Versions

- **`1.5.18`** — verified against live `agy 1.5.18` conversation SQLite schema
  (`gen_metadata`, `steps`, `trajectory_metadata_blob`) and protobuf wire layout:
  - `gen_metadata.data` -> `f1` (wrapper) -> `d.3` (model_id), `d.4` (primary usage),
    `d.9.4` (timestamp), `d.17` (repeated retry attempts, each `f2` = usage),
    `d.19` (model name), `d.21` (display name).
  - `steps.metadata` -> `f1`/`f8` (timestamps), `f9` (primary usage),
    `f24` (model info: `f1` id, `f8` display, `f12` name), `f28` (repeated retries).
  - Usage message -> `f1` (model_id), `f2` (uncached input), `f3` (output),
    `f4` (cache_write), `f5` (cache_read), `f9` (reasoning), `f10` (non-reasoning output),
    `f7`/`f11`/`f12` (request identity strings for union-find deduplication).

## Golden Cases

- `multi_call_verified.golden.json`: multi-call DB with reasoning, cache read,
  a retry on a different model with cache write, and transitive union-find
  duplicate-ID merging across `steps` and `gen_metadata`.
- `unknown_version.golden.json`: same DB decoded with an unpinned `agy` version
  (`status: "unverified"`).
- `malformed_blob.golden.json`: DB containing a valid row plus a truncated
  protobuf blob (`status: "malformed"`, malformed record has no tokens).

To regenerate after an intentional contract change:

```bash
UPDATE_GOLDEN=1 go test -buildvcs=false ./pkg/trajectory/agydecode/...
```
