**WIP — DO NOT MERGE YET.** Opened early for durability, not for review. The matcher rework in
item 3 below is not done. See "What is not finished".

Follow-up to `GoogleCloudPlatform/scion`#1195 (phase 0 Helm chart), which merged at 13:13:34Z
today. Two of the defects below are **live in `main` right now** and were found by rendering the
merged artifact rather than by reading it.

## Why this is not a lint change

`deployment.yaml` makes one call to `assertNoCredentialsInValues`, which walks **all of `.Values`**
recursively. Two holes in that walk let credential material reach a world-readable field.

### 1. A credential in a map KEY is not checked at all (disclosure)

The walk ranges `$k, $e` over maps and recurses only on `$e`. The key is never inspected.

Rendered against merged `main`, then against this branch:

| arm | `main` | this branch |
|---|---|---|
| `hub.podAnnotations.<credential>=x` | **renders clean** | refused |
| `hub.podAnnotations.-----BEGIN RSA-----=x` | **renders clean** | refused |
| control: same token as a *value* | refused | refused |
| control: `hub.podAnnotations.owner=platform-team` | renders | renders |

The rendered Deployment carries the credential as an annotation key. **Pod annotations are readable
by anyone with pod read access, which is a wider audience than the Secret's own RBAC** — the same
disclosure class as #1070, which is what this chart's credential guard exists to prevent.

### 2. A `/` in a URL password silences the userinfo guard (disclosure, on argv)

The password character class was `[^/@[:space:]]+`, so a slash left the pattern no route to the
terminating `@` and the check went quiet. Rendered against merged `main`:

| `--upstream=` value | `main` |
|---|---|
| `postgres://u:a/b@10.0.0.1/scion` | **renders — password on argv** |
| `redis://:S3cr3t/Xy@10.0.0.1:6379` | **renders — password on argv** |
| `mongodb://u:p/w@h1:27017/db` | **renders — password on argv** |
| `amqp://u:p/w@h:5672/vhost` | **renders — password on argv** |
| control: the same password with the slash removed | refused |

**The protection was anti-correlated with the mistake.** An operator who percent-encodes their DSN
correctly carries no raw slash and was protected; the operator who does not is carrying the slash,
and was the one let through.

The fix detects by **structure**, not by guessing which characters a password may contain: userinfo
is what sits between `://` and the last `@` of a URI authority, and the authority ends at `/`, `?`,
`#` or end-of-string. The class now excludes only `?` and `#`, which are the grammar's own
delimiters rather than a judgement about the value.

**Disclosed regression, pinned as a test row rather than omitted:** `https://example.com:8080/a@b/c`
— explicit port plus an `@` in the path — is now refused, and it is a legitimate URL. The port reads
as the password and the path segment as the host. Seven silent leaks traded for one loud refusal.
`tests/render-guards.sh` pins it with a comment saying the row asserts a **defect** and must be
flipped, not deleted, if anyone reclaims it.

The redaction pattern is deliberately the detector **minus** the authority-terminator tail, making
it a strict superset. If the two ever drift apart, `regexReplaceAll` returns the value unchanged and
the failure message prints the password into CI logs while still correctly refusing. There is an
assertion for that, using a slash-bearing password, because the slash is what the detector was
widened for.

## What is not finished

**3. The matcher itself still carries length floors, and floors have been ruled dead.**
`sk-[A-Za-z0-9_-]{16,}` and friends cannot work: `sk-proj-9xQvMKp2LrTdW7YbN4hJc0FgZ8sAe1Ru` (a real
key format) and `sk-triton-inference-server-gpu-a100-pool` (an ordinary node pool) are the same
length, so no floor over any charset separates them, for any N. This branch still has them. They
come out before review, together with striking `(?i)` and repairing the `(^|=)` anchor to
`(^|[^A-Za-z0-9_-])`.

## Verification

`tests/run-all.sh`: **155/155 assertions across 4/4 scripts, 0 meta-failures.** Every table above is
a render on `helm v3.16.3+gcfd0749`, with a negative control in the same run so that a guard which
refused everything would fail the table rather than pass it.

Not claimed: a production false-positive count. Every false-positive corpus produced for this guard
so far was built against three scheduling surfaces, and the shipped artifact walks all of `.Values`
— so those denominators are a floor on exposure, not a measurement of it. That count is being taken
separately and is not in this PR.
