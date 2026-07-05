# Harness Provisioner Consolidation: `scion_harness.py` as a Real Library

Status: IMPLEMENTED — all work packages (WP-0 through WP-5) landed on
branch `scion/harnesslib-refactor`. See commit log for per-WP details.
Author: fable-arch agent
Date: 2026-07-05
Baseline: main @ 83128b8

## 1. Current state

Seven harness bundles (`harnesses/{antigravity,claude,codex,copilot,gemini-cli,hermes,opencode}`)
each ship a `provision.py` (358–928 lines) and a `capture_auth.py` (~179 lines),
all stdlib-only, run inside the container by
`sciontool harness provision --manifest ~/.scion/harness/manifest.json`.

A shared helper **already exists**: `pkg/harness/embeds/scion_harness.py`
(214 lines). The Go host (`ContainerScriptHarness.Provision` →
`writeSharedHarnessHelper`) stages it next to `provision.py` in the bundle at
provision time, and every provision.py does:

```python
sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
try:
    import scion_harness
except ImportError:
    scion_harness = None   # + inline fallback code paths
```

So the injection mechanism the refactor needs is already in place — the
problem is the library is thin (path/JSON/MCP helpers only), treated as
optional, and everything that matters is still copy-pasted.

## 2. Findings

### 2.1 Duplication (~40–60% of every script)

Re-implemented verbatim (or near-verbatim) in all seven provision.py files:

- `_expand`, `_load_json`, `_write_json` (atomic tmp+rename)
- `_present_env_keys`, `_present_file_paths`, `_env_secret_files`, `_read_secret`
- auth-candidates loading, no-auth-mode gating
- `resolved-auth.json` / `env.json` output writing
- `_read_mcp_servers_inline` (fallback duplicate of the lib function it shadows)
- the entire `main()` / `_dispatch()` / argparse / manifest-load scaffold (~60 lines each)

That is roughly 1,400–1,700 duplicated lines across the seven scripts.

### 2.2 The copies are already drifting (this is the real cost)

Divergences found that look accidental rather than intentional:

| Concern | Variants observed |
|---|---|
| Secret trailing-whitespace | claude: `.strip()`; codex/opencode/hermes: `rstrip("\r\n")`; copilot/antigravity additionally fall back to `os.environ` |
| No-auth gate | claude/gemini/codex/opencode/antigravity: `if not candidates`; hermes: `if not env_keys`; copilot: `if not env_keys` **plus** a fallback-to-none on `ValueError` |
| env.json secret policy | claude/gemini write `${VAR}` placeholders; copilot/hermes/opencode write **raw secret values** into env.json |
| Managed-block markers | codex/hermes: `<!-- BEGIN SCION MANAGED <NAME> INSTRUCTIONS -->`; copilot: `<!-- SCION_MANAGED_BEGIN -->` with different merge semantics; antigravity: plain `shutil.copy2`, no managed block at all |
| Instruction projection | codex and hermes contain ~120 identical lines; copilot a third divergent implementation |
| Workspace resolution | claude/gemini: manifest `agent_workspace`; copilot: `SCION_WORKSPACE` env in one function and `SCION_AGENT_WORKSPACE` in another (likely a bug) |
| resolved-auth.json keys | ad-hoc per harness (`auth_file_written`, `vertex_ai`, `vertex_project_env`, …) |

Each of these is a place where a bug fixed in one harness silently stays broken
in the other six.

### 2.3 The optional-import anti-pattern

Every script guards `import scion_harness` with try/except and carries an
inline fallback (`_read_mcp_servers_inline`, inline MCP merge in claude). But
the host **always** stages the lib; an ImportError means the staging contract
was violated and should be a loud provisioning failure, not a silent switch to
a second, less-tested code path. The fallbacks double the MCP code and are
themselves drifting.

### 2.4 capture_auth.py is seven copies of one script

The six non-antigravity copies differ **only in their docstring** (verified by
diff). The script is already fully config-driven via
`inputs/capture-auth-config.json`. Antigravity's variant adds keyring
handling. There is no reason for six identical checked-in copies.

### 2.5 Test coverage is fragmented

Only codex and hermes have `provision_test.py`. Auth selection — the
safety-critical path — is untested in five of seven harnesses, and each test
file re-invents its own fixtures.

### 2.6 Version skew is unmanaged

`scion_harness.py` ships with the **scion binary** (Go embed), while
`provision.py` ships with the **installed harness-config** (which may have
been installed from an older bundle or directly from GitHub main). Today the
lib surface is small enough that skew is harmless; as soon as it becomes a
real library, an old binary staging an old lib under a new provision.py (or
vice versa) becomes the main failure mode. There is no version handshake
beyond `provisioner.interface_version: 1` in config.yaml, and no
feature-detection convention.

## 3. Proposal

### 3.1 One library, injected — grow `scion_harness.py` into the real API

Keep a **single source file** (stdlib-only, one file, no package). Grow it
from 214 lines into a layered toolkit, so a provision.py shrinks to only
harness-native logic:

```python
# provision.py after refactor (shape)
import scion_harness as sh

AUTH = sh.AuthSpec(
    harness="claude",
    methods=[
        sh.env_method("api-key", any_of=["ANTHROPIC_API_KEY"]),
        sh.env_method("oauth-token", any_of=["CLAUDE_CODE_OAUTH_TOKEN"]),
        sh.file_method("auth-file", path="~/.claude/.credentials.json"),
        sh.env_method("vertex-ai", all_of=["GOOGLE_CLOUD_PROJECT"],
                      any_of=["GOOGLE_CLOUD_LOCATION", "GOOGLE_CLOUD_REGION"]),
    ],
)

def provision(ctx: sh.ProvisionContext) -> None:
    auth = ctx.select_auth(AUTH)            # uniform explicit/no-auth/error handling
    _update_project_paths(ctx)              # harness-native, stays local
    if auth.method == "api-key":
        _apply_api_key_approval(ctx, auth)  # harness-native, stays local
    ctx.apply_mcp_simple(CLAUDE_MCP_MAPPING)
    ctx.write_outputs(auth, env=_env_overlay(ctx, auth))

if __name__ == "__main__":
    sh.run("claude", provision)             # argparse, manifest, dispatch, exit codes
```

Library layers (all additive to the existing file):

1. **`run(name, fn)` scaffold** — argparse, manifest load/validate, command
   dispatch, exception→exit-code mapping, consistent `"<name> provision:"`
   stderr prefixes. Replaces ~60 lines per script.
2. **`ProvisionContext`** — lazily-loaded accessors for bundle dir, workspace,
   inputs (auth-candidates, telemetry, mcp-servers, instructions/system
   prompt), secrets (`read_secret` with ONE whitespace policy), harness_config,
   outputs paths.
3. **Declarative auth engine** — `select_auth(spec)` implementing the shared
   semantics exactly once: explicit-type validation, precedence order,
   env/file/secret-file presence checks, the no-auth gate, uniform actionable
   error messages. This is the highest-value consolidation: 7 divergent
   copies of the most safety-critical logic become data.
4. **Standard outputs writer** — one `resolved-auth.json` schema (v2, with an
   `extra` escape hatch) and one env.json policy.
5. **Instruction projection** — one managed-block implementation (markers,
   strip-with-unclosed-guard, `# System Instruction` / `# Agent Instructions`
   / skills sections) parameterized by target path and marker label. Replaces
   the codex/hermes/copilot triplication and gives antigravity managed-block
   safety for free.
6. **MCP** — keep `read_mcp_servers` / `apply_mcp_servers_simple`; add
   `apply_mcp_translated(bundle, translate_fn, write_fn)` capturing the
   warn-and-skip loop skeleton that codex/opencode/copilot/hermes each
   duplicate around their genuinely-native `translate` functions.
7. **Config-file edit helpers** — atomic text write, TOML section
   strip/append (codex needs it twice: `[otel]` and `[mcp_servers.*]`),
   JSON-with-comment-lines reader (copilot's config.json).

Explicitly **out** of the lib: anything harness-native (Claude's
customApiKeyResponses, Codex TOML otel layout, AGY onboarding pre-staging,
Hermes .env format). The lib owns *plumbing and policy*; harnesses own
*translation*.

### 3.2 Make the import mandatory

Delete every `try/except ImportError` and every inline fallback. If the lib
is missing, exit 1 with a staging-bug message. One code path, half the MCP
code, no silently-diverging fallbacks.

### 3.3 Versioning contract

- Lib declares `INTERFACE_VERSION: int` (start at 2).
- Within a version: **additive-only** (new functions, new keyword args with
  defaults). Breaking change ⇒ bump, and bump `provisioner.interface_version`
  in config.yaml so the Go host can refuse/warn on mismatch.
- provision.py asserts once at import:
  `assert sh.INTERFACE_VERSION >= 2`, producing an actionable error
  ("scion binary too old for this harness bundle — upgrade scion or reinstall
  the bundle").
- Host writes the staged lib version into `manifest.json` for diagnostics.

### 3.4 Distribution: canonical source + per-bundle vendored copies (DECIDED)

Decision (2026-07-05): bundles must be **self-contained at the directory
level**, because (a) a harness can be installed from a direct URL to its
subdirectory (e.g. `github.com/GoogleCloudPlatform/scion/tree/main/harnesses/codex`)
and (b) third-party harness implementations in external repos (e.g.
`ptone/cogo`) copy `scion_harness.py` into their own tree. So the model is
**vendored copies, generated from one canonical source**:

- **Canonical source:** `harnesses/scion_harness.py` (moved from
  `pkg/harness/embeds/`). This is the only file humans edit; it carries the
  unit tests, `INTERFACE_VERSION`, and a `LIB_VERSION` (date-stamped) marker.
- **Vendored copies:** `harnesses/<name>/scion_harness.py`, one per bundle,
  regenerated by a `go generate` / make target that byte-copies the canonical
  file (plus a `# GENERATED — do not edit; source: harnesses/scion_harness.py`
  header).
- **Drift gate:** a Go test (files are already embedded via `harnesses/embed.go`)
  asserts every vendored copy is byte-identical to the canonical source.
  A stale copy fails CI; the fix is re-running the generator.
- **Staging precedence in the host:** if the installed harness-config dir
  contains `scion_harness.py`, stage *that* copy (bundle wins — preserves
  self-containment and deterministic third-party behavior); otherwise fall
  back to the binary-embedded copy (legacy installs keep working).
- **Third-party contract:** external harnesses pin whatever copy they vendored.
  The additive-only policy within an `INTERFACE_VERSION` (§3.3) is what makes
  an old vendored lib safe to run under a newer scion binary — the binary
  never *imports* the lib, it only stages and executes the bundle as a unit,
  so the only hard compatibility surface is the manifest/inputs/outputs file
  contract, which is versioned by `provisioner.interface_version` in
  config.yaml. Document both knobs in the "writing a new harness" guide.
- **Vendor vs. injection is an explicit, declared choice** (2026-07-05):
  third-party authors technically get both options via the staging
  precedence, but the choice must be *declared*, not inferred from file
  presence. config.yaml gains `provisioner.lib: vendored | injected`
  (new configs default to `vendored`; absent field = `injected` for legacy
  compatibility). Host behavior:
  - `vendored` + file present → stage the bundle's copy;
    `vendored` + file missing → **hard error** (packaging bug — do not
    silently substitute the binary's lib, which may behave differently than
    what the author tested).
  - `injected` → stage the binary-embedded copy; the author opts into
    tracking the binary's lib and the additive-API risk that entails.
  - Either way, the host logs which copy was staged and its `LIB_VERSION`
    in provisioning output, so skew questions are answerable from logs.
  Documented best practice for third parties: **vendor explicitly** — it pins
  the semantics the author actually tested, keeps failures at CI time in the
  author's repo instead of agent-start time in an end user's environment,
  and updating is a deliberate file copy with a changelog diff.

The binary-embedded copy remains only as the legacy fallback and for
`capture_auth.py` default staging (§3.5).

### 3.5 capture_auth.py: one implementation, vendored shims

The script is already config-driven. The logic moves into the lib as
`capture_auth_main()`; each bundle's `capture_auth.py` becomes a ~10-line
vendored shim calling it (keeps direct-URL installs self-contained — see
§3.4). The six identical copies disappear as real code; antigravity keeps
its keyring-aware override (rebased on the lib where trivial). See WP-4.

### 3.6 Testing strategy

- **Lib unit tests** (table-driven): the auth engine gets exhaustive coverage
  once — explicit selection, precedence, no-auth gate, missing-secret errors.
  Lives next to the lib, runs in CI with plain `python3 -m unittest`.
- **Per-harness tests**: shrink to harness-native translation only (MCP
  shapes, settings writes, TOML output) using a shared fixture helper from a
  `harnesses/testutil.py` (test-only, not staged).
- **Bundle contract test (Go)**: a single Go test iterating every directory in
  `harnesses/`, staging bundle + lib into a temp home, running provision.py
  against golden manifest/candidates fixtures, asserting
  `outputs/resolved-auth.json` and exit codes. Every future harness gets
  baseline coverage for free; drift becomes a CI failure.
- **Anti-fragmentation lint**: CI check that no provision.py defines the
  lib-owned helpers (`def _expand`, `def _load_json`, `def _read_secret`, …)
  or the ImportError guard. Prevents regression to copy-paste.

## 4. Behavior normalization decisions (fold into Phase 1)

Each drift point in §2.2 has an explicit answer; the owning work package
applies it (no other WP may change these behaviors):

1. Secret whitespace: `rstrip("\r\n")` only (preserve intentional inner/leading
   whitespace; a trailing newline is a file artifact). Implemented in the lib
   (WP-1); behavior change lands for claude in WP-3b.
2. env.json policy: **placeholders (`${VAR}`) by default**; raw values only
   where the consumer genuinely cannot expand env (documented per harness,
   0600, deliberate). Lands in WP-3c (opencode), WP-3d (hermes),
   WP-3e (copilot — with the verification caveat in its notes).
   Decided 2026-07-05: rides Phase 2/WP-3, no expedited fix.
3. No-auth gate: standardize on "no candidates staged AND no_auth declared",
   plus copilot's graceful fallback-on-ValueError **as an opt-in flag**
   (`fallback_to_none_on_error`) in the AuthSpec (a real requirement for
   hub-registered configs). Lib behavior in WP-1; per-harness in WP-3x.
4. Managed-block markers: one marker pair, `<!-- BEGIN SCION MANAGED -->` /
   `<!-- END SCION MANAGED -->`, with the lib accepting legacy variants when
   stripping (`<!-- BEGIN SCION MANAGED CODEX INSTRUCTIONS -->`,
   `<!-- BEGIN SCION MANAGED HERMES INSTRUCTIONS -->`,
   `<!-- SCION_MANAGED_BEGIN -->`) so reprovision of existing agents stays
   clean. Lib in WP-1; adopted in WP-3d/e/f.
5. Workspace: manifest `agent_workspace` is the single source; env fallback
   removed (fixes copilot's SCION_WORKSPACE/SCION_AGENT_WORKSPACE split).
   Lands in WP-3e.

## 5. Implementation work packages

This section is written for **implementing agents working in fresh,
independent sessions**. Each work package (WP) is self-contained: it restates
the context it needs, lists preconditions, concrete steps, files, acceptance
criteria, and verification commands. Do not assume access to the design
conversation — this document plus the repo is the full input.

### 5.0 Orientation (read first, every WP)

- Harness bundles live in `harnesses/<name>/` (antigravity, claude, codex,
  copilot, gemini-cli, hermes, opencode). Each has `config.yaml`,
  `provision.py`, `capture_auth.py`, and is embedded into the scion binary via
  `harnesses/embed.go`.
- At agent start, the Go host (`pkg/harness/container_script_harness.go`)
  stages the bundle into the container at `$HOME/.scion/harness/` — including
  `provision.py`, `config.yaml`, the shared helper `scion_harness.py`
  (currently from `pkg/harness/embeds/scion_harness.py` via
  `pkg/harness/scion_harness_embed.go`), a `manifest.json`, and inputs
  (`inputs/auth-candidates.json`, `inputs/mcp-servers.json`,
  `inputs/telemetry.json`, `inputs/instructions.md`,
  `inputs/system-prompt.md`, `inputs/capture-auth-config.json`) and staged
  secrets under `secrets/<NAME>`.
- The container runs `python3 $HOME/.scion/harness/provision.py --manifest
  .../manifest.json`. The script writes `outputs/resolved-auth.json` and
  `outputs/env.json`. Exit codes: 0 ok, 1 error, 2 unsupported command.
- Scripts must remain **Python 3 stdlib-only, single-file** — they run in
  arbitrary container images that only guarantee `python3`.
- Hard rule for every WP: no behavior change unless this doc explicitly
  calls for it (§4 lists the sanctioned changes and which WP applies each).

Dependency graph:

```
WP-0 (goldens/contract test)  ──┐
WP-1 (canonical lib)          ──┼──► WP-3a..3g (per-harness, parallel) ──► WP-5
WP-2 (vendoring infra)        ──┘            WP-4 (capture_auth) ────────► WP-5
```

WP-0, WP-1, WP-2 are mutually independent and can run in parallel sessions.
Each WP-3x needs all of WP-0/1/2 merged. WP-4 needs WP-1+WP-2. WP-5 is last.

### WP-0 — Golden contract fixtures + bundle contract test

**Goal:** freeze current behavior of all 7 provision.py scripts so later WPs
can prove they changed nothing unintentionally.

**Steps:**
1. Create `pkg/harness/bundle_contract_test.go`: a Go test that, for every
   directory in `harnesses/` containing a `provision.py`, per fixture case:
   creates a temp `$HOME`, stages the bundle dir + the shared helper
   (mirroring the staging in `container_script_harness.go`), writes the
   fixture `manifest.json` / `inputs/*` / `secrets/*`, runs
   `python3 provision.py --manifest ...` with `HOME` set to the temp dir,
   and asserts: exit code, full `outputs/resolved-auth.json`, full
   `outputs/env.json`, and (where the fixture declares them) named
   harness-native files (e.g. `~/.claude.json`, `~/.codex/config.toml`,
   `~/.gemini/settings.json`).
2. Fixture cases per harness, under
   `pkg/harness/testdata/bundle_contract/<harness>/<case>/`:
   at minimum `api_key`, `no_auth_mode`, `no_creds_error`, `explicit_invalid`,
   plus per-harness methods (oauth-token, auth-file, vertex-ai) where
   supported, and one `mcp_servers` case each.
3. Capture goldens from **current** behavior (generate, then eyeball). Where
   current behavior is one of the known divergences in §2.2, add a
   `// DIVERGENCE(<topic>): scheduled to change in WP-3x per §4.n` comment in
   the golden or test table so later WPs update goldens deliberately.
4. Skip the test with a clear message when `python3` is absent.

**Acceptance:** `go test ./pkg/harness/ -run TestBundleContract` green on a
clean checkout; every harness has ≥4 cases; goldens reviewed, divergences
annotated. No production code changed.

### WP-1 — Grow the canonical library

**Goal:** expand `pkg/harness/embeds/scion_harness.py` (keep this path for
now; WP-2 moves it) from 214 lines of helpers into the full API of §3.1,
purely additively, with stdlib-only unit tests.

**Steps:**
1. Add to `scion_harness.py` (single file, stdlib-only, `from __future__
   import annotations`):
   - `INTERFACE_VERSION = 2`, `LIB_VERSION = "<YYYY-MM-DD>"`.
   - `run(harness_name, provision_fn)` — argparse (`--manifest`), manifest
     load/shape-check, command dispatch (`provision` → fn, else exit 2),
     `ProvisionError` → stderr + exit 1, consistent
     `"<name> provision:"` message prefix helpers (`ctx.info/warn`).
   - `class ProvisionContext` — properties: `harness_name`, `manifest`,
     `bundle_dir`, `inputs_dir`, `workspace` (manifest `agent_workspace`,
     default `/workspace`), `home`, `harness_config`, `candidates`
     (auth-candidates.json), `explicit_type`, `env_keys`, `file_paths`,
     `env_secret_files`, `file_secret_files`, `telemetry` (telemetry.json),
     `model_resolution`; methods: `read_secret(name)` (policy:
     `rstrip("\r\n")` — §4.1; optional `env_fallback=False` kwarg),
     `read_input_text(name)`, `output_paths()`.
   - Auth engine: `AuthSpec` / method constructors (`env_method`,
     `file_method` — presence via mounted candidate paths, disk, or
     `file_secret_files` key) and `ctx.select_auth(spec)` implementing:
     explicit-type validation with the uniform error format, precedence =
     list order, the no-auth gate (§4.3: no candidates staged AND
     `harness_config.no_auth.behavior` set → method `"none"`; plus opt-in
     `fallback_to_none_on_error` flag for the copilot semantics), and
     no-creds `ProvisionError` composed from the spec's per-method hints.
     Returns a `ResolvedAuth` (method, env_key, spec entry).
   - `ctx.write_outputs(resolved, env=..., extra=...)` — standard
     resolved-auth.json (schema_version 2: harness, method, explicit_type,
     env_var?, auth_file?, plus `extra` merged) and env.json.
   - Instruction projection: `project_instructions(ctx, target,
     marker_label=None, include_skills=True, system_prompt_mode=...)` —
     one implementation of the managed-block compose/strip (accepting the
     legacy marker variants listed in §4.4 when stripping).
   - `apply_mcp_translated(ctx, translate_fn, write_fn)` — the shared
     warn-and-skip loop (sorted names, scope demotion warning, skip on None).
   - TOML helpers: `toml_escape`, `toml_inline_table`, `toml_string_array`,
     `strip_toml_sections(content, header_predicate)` (generalizing the
     codex `[otel]` / `[mcp_servers.*]` strippers, preserving their
     blank-line trimming), `atomic_write_text(path, content, mode=None)`.
   - `read_json_skipping_comment_lines(path)` (copilot config.json).
   - `capture_auth_main(argv=None)` — port of the (identical) capture_auth
     logic, exit codes 0/1/2/3 preserved.
2. Unit tests in `pkg/harness/embeds/scion_harness_test.py` (plain
   `unittest`, no third-party deps), table-driven over the auth engine
   (explicit/precedence/no-auth/errors), outputs writer, projection
   (compose + strip + legacy markers + unclosed-marker guard), TOML strip,
   secret whitespace policy.
3. Wire the Python unit tests into CI via a small Go test that shells out to
   `python3 -m unittest` (skip when python3 absent), e.g. in
   `pkg/harness/scion_harness_embed_test.go`.

**Constraints:** additive only — existing functions keep exact signatures and
behavior; no harness `provision.py` is modified; WP-0 goldens (if already
merged) stay green.

**Acceptance:** `python3 -m unittest` green for the new test file;
`go test ./pkg/harness/` green; lib remains one file, stdlib-only.

### WP-2 — Vendoring infrastructure + declared lib mode

**Goal:** implement §3.4: canonical file at `harnesses/scion_harness.py`,
generated per-bundle copies, drift gate, and the explicit
`provisioner.lib: vendored | injected` staging behavior.

**Steps:**
1. `git mv pkg/harness/embeds/scion_harness.py harnesses/scion_harness.py`
   (and its unit test alongside); point
   `pkg/harness/scion_harness_embed.go`'s embed at the new location (embed
   directives can't reference parent dirs — either move the embed var into
   package `harnesses` next to `harnesses/embed.go` and reference it from
   `pkg/harness`, or add a tiny embed shim package; keep
   `writeSharedHarnessHelper`'s behavior identical).
2. Generator: `go run ./harnesses/gen` (or a Makefile target invoked the
   same way CI does) that copies the canonical file into each
   `harnesses/<name>/scion_harness.py`, prepending
   `# GENERATED FILE — DO NOT EDIT. Source: harnesses/scion_harness.py`.
   Commit the generated copies.
3. Drift-gate test (Go, e.g. `harnesses/vendored_lib_test.go`): every
   `harnesses/*/scion_harness.py` == canonical + header, byte-exact.
4. config.yaml schema: add `provisioner.lib` (`vendored` | `injected`;
   absent → `injected`) in `pkg/config` (see `settings_v1.go` and wherever
   `provisioner.*` is parsed). Set `lib: vendored` in all 7 bundle
   config.yaml files.
5. Staging precedence in `ContainerScriptHarness.Provision`:
   - `vendored`: stage `<config_dir>/scion_harness.py`; if missing → hard
     provisioning error naming the file and the `provisioner.lib` setting.
   - `injected` (or absent): stage the binary-embedded copy (today's
     behavior, unchanged).
   - Both: log staged source + `LIB_VERSION` (parse the
     `LIB_VERSION = "..."` line) at info level.
6. Ensure `scion harness-config install` copies `scion_harness.py` as part
   of the bundle dir (verify against `pkg/harness/bundle_install*.go`; it
   should already, since it copies the directory — add a test).

**Acceptance:** drift-gate test green; `go test ./...` green; WP-0 goldens
(staging now uses vendored copies) unchanged; a config.yaml with
`lib: vendored` and a deleted vendored file produces the hard error in a
unit test.

### WP-3a..g — Per-harness migration (one agent per harness, parallel)

**Goal:** rewrite each `harnesses/<name>/provision.py` on the WP-1 API.
Common contract for every WP-3x:

- Delete: the `try/except ImportError` guard and ALL fallback duplicates
  (`_expand`, `_load_json`, `_write_json`, `_present_env_keys`,
  `_present_file_paths`, `_env_secret_files`, `_file_secret_files`,
  `_read_secret`, `_read_mcp_servers_inline`, inline MCP merges, the
  argparse/`main`/`_dispatch` scaffold). Import `scion_harness` directly;
  add `assert scion_harness.INTERFACE_VERSION >= 2` with an actionable
  message.
- Keep: everything harness-native, as local functions taking `ctx`.
- Apply the §4 normalizations that name your harness; update the WP-0
  goldens annotated `DIVERGENCE` for your harness in the same PR, with the
  doc reference in the commit message. All other goldens must be untouched.
- Acceptance (identical for all): `go test ./pkg/harness/ -run
  TestBundleContract/<harness>` green; full `go test ./...` green; drift
  gate green; provision.py contains none of the deleted helper names
  (WP-5 automates this check, verify manually until then).

Per-harness notes:

| WP | Harness | Size today | Specific notes |
|----|---------|-----------|----------------|
| 3a | gemini-cli | 358 | Simplest; no lib import today. Native: `_GEMINI_AUTH_TYPE_MAP` + settings.json `security.auth.selectedType` write. §4.2: its env overlay already uses placeholders — keep. |
| 3b | claude | 556 | Native: project-path rewrite, customApiKeyResponses fingerprint, MCP mapping dict (keep `apply_mcp_servers_simple`). §4.1 applies (secret whitespace). |
| 3c | opencode | 496 | Native: MCP local/remote translation (`apply_mcp_translated`). §4.2 applies: vertex env overlay switches raw values → placeholders. Preserve the `gcp_metadata_mode` reserved guard as a spec flag or documented TODO. |
| 3d | hermes | 534 | Native: `~/.hermes/.env` writer, HERMES_* env overlay, model_resolution passthrough, mcp.json (incl. stale-file removal), instruction projection → lib call. §4.2 applies (env.json). Has `provision_test.py` — port to lib API. |
| 3e | copilot | 627 | Native: copilot-instructions.md target, mcp-config.json, settings/config.json defaults. Fixes: §4.5 workspace (use `ctx.workspace`, delete `SCION_WORKSPACE`/`SCION_AGENT_WORKSPACE` reads), §4.2 (token → placeholder IF the runtime env projection delivers the var to the copilot process — verify against `pkg/harness` env plumbing first; if copilot only receives it via env.json, document the exception in provision.py and keep raw+0600), §4.3 (its fallback-on-error becomes `fallback_to_none_on_error=True`). Keep the os.environ candidate fallback as an explicit spec option (hub-registered configs need it). |
| 3f | codex | 928 | Largest. Native: auth.json writers (apikey + CODEX_AUTH secret), otel TOML build (use lib TOML helpers + `strip_toml_sections`), `[mcp_servers.*]` emission, instruction projection → lib call. Has `provision_test.py` — port. Byte-equivalence of config.toml output with the Go implementation (`codex_config.go`) must be preserved — the goldens cover it. |
| 3g | antigravity | 731 | Partial migration only: adopt run/ctx/select_auth/write_outputs and delete duplicated helpers; keyring wrapper generation, onboarding pre-staging, hooks.json, chown logic stay native and untouched. Its `AGY_TOKEN`/os.environ special cases become spec flags (`env_fallback`) rather than ad-hoc code where the lib supports it. |

### WP-4 — capture_auth consolidation

**Preconditions:** WP-1 (lib has `capture_auth_main`), WP-2 (vendoring).

**Steps:** replace the six identical `harnesses/{claude,codex,copilot,
gemini-cli,hermes,opencode}/capture_auth.py` with the same ~10-line shim:
shebang + license + `import scion_harness; sys.exit(
scion_harness.capture_auth_main())` (bundle-dir sys.path insert as in
provision.py). Antigravity keeps its keyring-aware script but rebases its
generic half on `capture_auth_main` where trivial. Verify the Go side stages
capture_auth.py from the bundle dir unchanged
(`StageCaptureAuthAssets`). Add/extend a contract test case invoking the
shim with a fixture `capture-auth-config.json` asserting exit codes 0/2/3.

**Acceptance:** all capture_auth flows keep exit-code semantics; `go test
./...` green.

### WP-5 — Guardrails and documentation

**Preconditions:** all WP-3x + WP-4 merged.

**Steps:**
1. Anti-fragmentation check (extend the drift-gate test): fail if any
   `harnesses/*/provision.py` matches `def _expand(`, `def _load_json(`,
   `def _write_json(`, `def _read_secret(`, `def _present_env_keys(`,
   `_read_mcp_servers_inline`, or `except ImportError` around
   `scion_harness`.
2. Rewrite `harnesses/README.md`: add a "Writing a new harness" guide —
   bundle layout, the provision.py skeleton on the lib API, the vendoring
   generator, `provisioner.lib` semantics, INTERFACE_VERSION /
   `provisioner.interface_version` rules for third parties, and the golden
   fixture requirements (a new harness must ship contract fixtures).
3. Update this doc's status to IMPLEMENTED with links to the landed PRs.

**Acceptance:** lint catches a seeded violation in a test; README guide
reviewed; `go test ./...` green.

Expected end state: ~4,000 lines of provision.py shrink to roughly 1,500
(harness-native only) over a ~600-line library with real tests; six
capture_auth.py copies become shims; every shared behavior defined and
tested in exactly one place; drift structurally prevented.

## 6. Decision log (all resolved — implementers should treat these as final)

1. Distribution model — RESOLVED 2026-07-05: vendored per-bundle copies
   generated from a canonical `harnesses/scion_harness.py`, CI drift gate,
   explicit `provisioner.lib: vendored | injected` with vendored-but-missing
   as a hard error (§3.4). Owner: WP-2.
2. Name — RESOLVED: keep `scion_harness.py` (renaming to `harnesslib.py`
   would break the staged import name for existing installs for zero
   functional gain).
3. env.json raw→placeholder timing — RESOLVED 2026-07-05: rides the WP-3
   per-harness migration; no expedited standalone fix (§4.2).
4. capture_auth distribution — RESOLVED: vendored thin shim per bundle
   calling `scion_harness.capture_auth_main()` (self-contained for
   direct-URL installs), superseding the earlier host-staged-default idea.
   Owner: WP-4.

If an implementing agent believes a decision here is wrong or hits a blocker,
stop and escalate to the coordinating agent rather than deviating silently.
