# gs:// link fetch-and-render — image previews, SVG-as-source, and the recent-files exclusion

Content typing and preview classification for the gs:// link fetch-and-render
endpoint and its web preview.

On the hub side, the endpoint's Content-Type decision recognizes PNG, JPEG,
GIF and WebP from `http.DetectContentType` on the sniffed byte prefix the
endpoint already reads, before falling back to the text-or-octet-stream
decision — never from the object's extension or its stored metadata. An
object with a `Content-Encoding` is always served as octet-stream regardless
of what its compressed bytes happen to sniff as, since that check runs first.
Other image types the standard sniffer knows (BMP, Windows icons) are not on
the allow-list. `http.DetectContentType` never reports `image/svg+xml`, so an
SVG object is never classified as an image by construction, and a
`.png`-named object whose actual bytes are HTML sniffs as plain text, never
as an image and never as `text/html`.

On the web side, a gcs preview target is classified from the real response
rather than in advance: the client fetches once, then decides image vs.
"can't be previewed" vs. inline text from the response's own `Content-Type`
and `Content-Length` headers. An image requires both a recognized raster
extension *and* a Content-Type of exactly `image/png`, `image/jpeg`,
`image/gif` or `image/webp` — an SVG's extension is never in that recognized
set, so it can never render as `<img>` regardless of Content-Type, and a
misnamed `.png` whose sniffed bytes are plain text falls through to the
text/code path instead. An `application/octet-stream` response, any other
`image/*` response, and a text response whose `Content-Length` exceeds the
512 KB inline-preview cap are each shown as their own fixed placeholder
message with Download still offered; each aborts the in-flight fetch before
its body is read, which matters most for the oversized-text case, where the
body could otherwise be up to the hub's 10 MiB cap. When the response carries
no usable `Content-Length`, the text that was read is held to the same 512 KB
cap.

The palette recent-files extractor has an add-only exclusion: a
`/workspace/...` or `/scion-volumes/...` substring embedded inside a
`gs://bucket/object` URI (an object literally named `workspace/...` sits
right after the bucket's own separator, forming that exact substring) is not
recorded as a local file. The existing extraction tests are unmodified and
pass.

The interplay between the gcs linkifier and the GitHub shortform-reference
pass has direct unit coverage alongside its real-Chromium counterpart. For a
fragment-shaped gs:// tail, neither pass produces a link at all — the gs://
continuation rule voids the occurrence outright, and the GitHub-ref
pattern's own boundary rule independently rejects the leftover text — which
also means the cross-feature collision (the GitHub-ref pass reaching inside
an already-linked gcs object's text) cannot arise for that vector shape: a
successful gcs link can never itself display the trailing characters that
pattern would need to match.

Docs: the hub server page's GCP identity material has a section on gs://
links in chat — what links and when, which service account is used, size
limits and preview types, and the uniform error behavior with its Cloud
Console fallback — cross-linked from the Native Web Chat feature list.

Verification: `go build`/`go vet`/`gofmt`/golangci-lint clean; the scoped
Go suite including the content-typing tests; `tsc --noEmit` clean for both
the main and e2e configs; the scoped vitest suites; a real-Chromium spec
covering every classification state, stress-tested; the extraction, linkify
and chat-palette suites; prettier clean on every changed file. Every
non-equivalent new or changed condition on both the Go and web sides is
caught by a named unit test when removed.
