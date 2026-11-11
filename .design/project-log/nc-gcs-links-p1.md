# gs:// link fetch-and-render, phase 1 — hub endpoint and web linkifier

Implemented `GET /api/v1/gcs/object`, following
the approved security model: the fetch always uses the sending
agent's current assigned GCP service account, resolved server-side from a message
the viewer is authorized to read, for a `gs://bucket/object` URI that literally
appears in that message's stored body. The client never names an agent, a service
account, or an email — any extra or duplicated query parameter is rejected before
any store lookup. Validation happens before rate limiting and concurrency admission,
which happen before any store lookup, which happens before token minting or a GCS
call, matching the design's step ordering exactly.

The endpoint's fourteen steps deny with a single byte-identical 404 body for every
"not visible" case (unknown/soft-deleted message, unreadable conversation, URI not
in the body, non-agent or forged sender, no usable SA, and a GCS-side 404/403/401
alike) — only the audit event's reason field distinguishes them. Separate statuses
exist for the cases a viewer already knows about: 400 for a malformed or
client-asserted-identity request, 429 for the per-viewer rate limit (60/min) or the
16-slot global concurrency semaphore, 413 for an object over the 10 MiB cap (checked
from `Attrs` before the reader ever opens), and 502 for a mint failure or a
generation-pinned read whose object changed underneath it. Generation pinning is
implemented as an `If(GenerationMatch)` read precondition rather than an explicit
generation address, so a concurrent overwrite reliably surfaces as a distinguishable
precondition failure regardless of whether the bucket has object versioning on.
Content-Type is decided from a sniffed byte prefix and the object's own
Content-Encoding, never from the object's stored metadata, so an object whose
metadata claims `text/html` is always served as `text/plain` or
`application/octet-stream`, with `nosniff`, the existing sandbox CSP, and an
always-`attachment` disposition.

The message-visibility check needed a non-writing core extracted from the existing
group-conversation authorization path so it could be reused without an
`http.ResponseWriter`; the existing group-authz test suite passes
unchanged, proving no behavior change for the routes that already used it.

The URI-in-body boundary check performs exact extraction equality: scanning the
stored body left to right across every bucket (not just the requested one), at
each left-boundary `gs://<bucket>/` occurrence it extracts the object exactly as
the web pattern would (the same maximal-run, trim-trailing-punctuation,
reject-on-trailing-slash, reject-on-fragment/query/param-continuation, and
reject-on-a-following-backslash logic on both sides) and allows only on an exact
match — never a substring boundary-scan on the requested object's own text,
which could not distinguish a posted `secret~` from a request for the prefix
`secret`. Each occurrence, matched or not, consumes its whole raw run before the
scan continues, mirroring the client regex's own match-consuming behavior, so a
`gs://` nested inside another occurrence's object run (including one naming a
different bucket) is never its own candidate. Because the client's linkifier
runs on marked's rendered, tag-split, HTML-escaped text while the server scans
the raw markdown body, the two sides run the identical algorithm on different
input; a small number of markdown-syntax-driven mismatches (an emphasis or
strikethrough wrapper, a `?` right before a tag, a raw HTML entity, a
markdown-escaped character) are accepted as UX-only — the mismatched link still
resolves to the uniform "not available" state plus the Cloud Console fallback,
never to unauthorized access — and are pinned with tests asserting the actual
behavior on each side.

The hub side is also gated by a registered experiment, `web.gcs_links`
(`pkg/experiments/registry.go`), checked at step 1 alongside the token
generator — either missing denies with the same uniform not-available
response, so a caller cannot tell which one is off. Capability flags that
change hub behavior belong in the experiments registry, not as an ad-hoc
`/api/v1/settings/public` boolean (AGENTS.md "Experimental features"); the
web client reads the same experiment through the standard
`GET /api/v1/experiments` path used by every other registered experiment,
rather than a dedicated settings field.

On the web side, `gs://bucket/object` is a new `ENTITY_PATTERNS` entry gated on
the `web.gcs_links` experiment and the message's real sender being an agent — a
user-sent message never links regardless of body content, and v2 chat's own "not
me" layout heuristic (used to decide left- vs. right-alignment) is kept separate
from this gate so another user's message in the same conversation cannot link
either. The experiment defaults off; enabling it is necessary but not
sufficient, since the hub endpoint also requires a configured token generator.
The pattern uses a captured
boundary group rather than a lookbehind assertion (kept out of `web/src` entirely,
per the existing constraint), and its object character class excludes `& < > " '`
and whitespace, so a
hostile object name can never inject a DOM attribute or element — proved both by a
regex-boundary unit test and by a real-Chromium spec, since the linkifier runs on
already-HTML-escaped text and a mocked, non-sanitizing renderer would give a false
pass. `<scion-chat-file-preview>` gained a `kind: 'gcs'` target: a reject-then-pin URL
builder (mirroring the existing attachment/path builders' reject-don't-normalize
rule), text/markdown/code rendering, and the uniform error texts per status code.

The token mint and storage client are per request, not cached: after every
authorization step passes, the handler mints a `devstorage.read_only` token
for the sender's current SA using the request's own context (so a hung mint
is bounded by the same deadline as everything else), then builds a
`*storage.Client` whose HTTP transport wraps that one token
(`oauth2.StaticTokenSource`) over a single shared, credential-free base
`http.Transport` — shared so independent requests still reuse pooled
connections, carrying no credentials of its own so it cannot leak one
request's token into another's calls. The client is closed when the request
finishes. Nothing is cached or deduplicated across requests: concurrent
requests for the same SA each mint their own token independently rather than
sharing one in-flight call.

A late-arriving scope addition (an "Open in Cloud Console" fallback link, since the
client cannot know ahead of a click whether the sender still has a usable service
account) was folded in before handoff: every non-400 gcs error state shows the
fallback, built entirely client-side from the already-visible URI, with no additional
hub call.

Verification: `go build`/`go vet`/`golangci-lint` clean; the scoped Go suite,
including a `-race` run of the full suite and of the concurrency, wait-site and
limit tests specifically; a real-client test against a minimal GCS JSON-API
subset (asserting the per-request minted Authorization header, no cross-SA
token reuse, generation pinning, and 404/403 mapping); the existing
group-authz, route-classification and experiments-registry suites unchanged;
`tsc --noEmit` clean for both the main and e2e configs; the scoped vitest
suites; a real-Chromium spec, stress-tested at `--repeat-each=20`, alongside
the existing extraction and chat-palette suites unchanged; prettier clean on
every changed file. A mutation pass against the hub endpoint's guard
conditions, the per-request mint path, the extraction/continuation helpers on
both sides, and the web gating logic confirmed each non-equivalent conditional
is caught by an existing or newly added test (three redundant
`pinGcsObjectUrl` key checks are equivalent mutants).

Deferred to phase 2, matching the design's phase split: image/SVG sniffing and
classification, the octet-stream "can't preview" state, the Content-Length
pre-fetch abort optimization, and the recent-files gs:// exclusion (not required in
phase 1 — leftmost-wins already keeps a gs:// link from also becoming a path link).
