# Federated project access item 6A: wording follow-up and merge of upstream main (06aa4b40c)

Date: 2026-10-13. Branch: scion/mmr-fed-p1-6a (ptone/scion#4797, tracked in ptone/scion#4603).

## Wording
- Comments and two log messages that item 6A added for the federation-token route now say
  "user access credential for a peer hub" instead of "federation user token". Identifiers,
  the `scion_token_kind` claim, config keys and the route path are unchanged. No test asserts
  the changed strings.

## Merge
- Item 1 is now on upstream main as a squash commit (GoogleCloudPlatform/scion#3489), with the
  same patch as the fork branch item 6A was built on. Merged upstream main at 06aa4b40c forward
  (no rebase).
- One conflict: `pkg/experiments/registry.go`, the federation.project_visitors comment and
  Description. Resolved to upstream's item 1 text plus item 6A's additions; the comment's
  "user tokens for peer hubs" became "user access credentials for peer hubs" in the resolution.
- Check: the diff from upstream main to the merged head equals the diff from
  (old base aec039c95 + the item 1 squash) to the pre-merge head, apart from that one comment line.

## To confirm in the gate run
- Item 6A's own tests (pkg/hub federation token tests, hubclient, cmd, config, authzop with
  `TestCatalogReportStaleness`) and the masked `./cmd/...` build at the merged head.
- Upstream removed the mattn SQLite test driver from the module (GoogleCloudPlatform/scion#3447);
  the pkg/hub shard compiles show whether item 6A's test helpers still build.
