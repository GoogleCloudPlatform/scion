# webperf A2: gzip JSON on the /api/v1/ web mount

Fork issue: ptone/scion#5304 (epic #5301). Design: webperf design section 4.2
and phase A2.

## What changed

- New package `pkg/httpcompress` (no `pkg/hub` dependency). `Wrap(next, Options)`
  gzips eligible responses:
  - Request-time bypass (the handler gets the original `ResponseWriter`): an
    `Upgrade` header or `Connection: upgrade`, `Accept` containing
    `text/event-stream`, `Options.BypassPath`, HEAD, and clients without a
    non-zero `gzip` in `Accept-Encoding`. A wildcard does not count as gzip.
  - Response-time decision, made once at the first WriteHeader, Write or Flush:
    only status 200/201/203, media type `application/json` or
    `application/problem+json`, no `Content-Encoding`, and `Content-Length`
    unset or at least MinSize (1 KiB). Anything else is passthrough for the
    rest of the response.
  - JSON without a Content-Length is held up to MinSize. A small body is sent
    as identity. Crossing the threshold, or calling Flush, starts compression;
    once over the threshold, the write goes straight to gzip with no copy into
    the hold buffer.
  - Flush flushes gzip and then the underlying writer. `Unwrap` supports
    `http.ResponseController`. `Hijack` is passed through unless the response
    is held or compressing.
  - Compressed responses drop `Content-Length`, set `Content-Encoding: gzip`,
    append `Vary: Accept-Encoding`, and weaken an ETag to `W/"...-gz"`.
    Writers are pooled at level 5.
- `pkg/hub/web.go` `MountHubAPI` wraps `sessionToBearerMiddleware(hubHandler)`.
  `isCompressBypassPath`, next to `isProxyRoute`, lists the paths that are
  never wrapped:
  - streaming and connection routes: agent PTY, every agent port route, and
    any path ending in `/stream` or `/events`;
  - auth, agent-facing `/api/v1/agent/`, agent token refresh, delegation
    exchange, broker registration, join and secret rotation, project
    registration, invites, test identities, and env and secrets routes;
  - agent item responses (`/api/v1/agents/{id}` and the project-scoped form).
  - responses with signed object-store or capability URLs (`isSignedURLPath`):
    agent workspace sync, template, user template, harness-config and skill
    upload and download, skill versions, artifact links and version views,
    and the artifact view and shared routes.
  `MountHubAPI` also sends non-GET requests to the create collections
  (`/api/v1/agents`, `/api/v1/projects/{projectId}/agents`, `/api/v1/templates`,
  `/api/v1/users/me/templates`, `/api/v1/harness-configs`, `/api/v1/skills`;
  matched by `isCreateCollectionPath`) to the hub handler without the wrapper,
  so create responses (agent config, signed upload URLs) stay uncompressed
  while the GET lists are compressed.
- `/events` and the SPA shell are mounted on the web mux, outside `/api/v1/`,
  so the wrapper never sees them.

## Tests

- `pkg/httpcompress/wrap_test.go`: every A2 acceptance name
  (`TestWrap_JSONCompressed`, `TestWrap_EventStreamPassthroughUnbuffered`,
  `TestWrap_UpgradeNotWrapped`, `TestWrap_SmallJSONUncompressed`,
  `TestWrap_FlushForcesDecision`, `TestWrap_PatchJSONControlNotBypassed`, and
  the rest), plus Accept-Encoding parsing, Vary, non-JSON and panic paths.
- `pkg/hub/web_api_compress_test.go`, all in one hubshard shard (6 at this head):
  - real hub handlers through `buildHandler()`: the agent list is gzipped,
    agent item GET/PATCH, agent create (POST, both collection forms) and
    template create/upload/download (signed URLs) responses are identity,
    small public JSON is
    identity, and the log and message streams deliver their first event
    unencoded before the handler ends;
  - PTY/tunnel upgrades, the port proxy, `/events` and the SPA shell;
  - non-2xx JSON passthrough;
  - every signed-URL route through the mount (large JSON stays identity,
    neighbouring routes are gzipped);
  - `TestCompressBypass_StreamRoutesFromCatalog`, which walks the authz
    catalog's SSE and websocket entry points and the agent sub-route table, and
    fails on any route the bypass does not cover.

## Learned

- Go's client transport adds `Accept-Encoding: gzip` and decodes transparently,
  and no hub client in the repo sets the header or reads a raw response
  `ContentLength`. So CLI traffic to a combo hub needs no change.
- `net/http` may compute a `Content-Length` for a short compressed body when
  the handler returns before the first chunk is flushed. Tests should assert
  "not the identity length", not "no Content-Length".
- The authz catalog's `/api/v1/agents/{id}/attach` websocket pattern exists
  only in a drift-test fixture. The live attach route is `/pty`.
- In zsh, `$names` is not word-split: pass `${=names}` to tools such as
  `hubshard.sh -locate`.
- pkg/hub tests are not compiled at dev time under the lane rules. They are
  verified in the 16G hubshard gate window and by fork CI.
