# Release Notes (2026-10-19)

This period focused on web client performance: compressed API responses, precompressed static assets, a lazily loaded terminal workspace and far fewer re-renders on busy projects. Server and CLI settings precedence was tightened, user access tokens gained group and user administration, and Kubernetes and Cloud Run sandbox agents report their state more accurately.

## ⚠️ BREAKING CHANGES
* **Server base URL and settings location** (#3849): On a server that runs the Hub, an explicit `--base-url` now outranks `SCION_SERVER_HUB_ENDPOINT`, then `SCION_SERVER_BASE_URL`, then `server.hub.public_url` from `settings.yaml`. The OIDC issuer and transport audience defaults follow the resolved value. Server startup settings load from the global directory only.
* **`SCION_HUB_PROJECT_ID` outranks file project IDs** (#3846): In both settings loaders the env var now beats the project-id file and a settings `hub.project_id`. Each distinct disagreement logs one warning naming the sources. `SCION_PROJECT_ID` keeps its rank.
* **Strict `SCION_SERVER_*` values** (#3788): Server startup rejects an unparseable boolean or number value and names the variable. Typed settings read `=false` as false, and legacy alias spellings log a one-time deprecation warning (names only, never values).
* **Image registry source order** (#3786, #3793): One resolver serves every read of `image_registry`, in this order: the Runtime Broker's node env (`SCION_IMAGE_REGISTRY`), Hub-stored, settings file, default. Within each level a profile beats the top level. A value is stored only when the field itself changes, so echoes no longer pin values. The Server Config page shows where the registry comes from, with registry credentials redacted.

## 🚀 Features
* **Faster web client** (#3837, #3835, #3828, #3826, #3858, #3830, #3831, #3827, #3829, #3832, #3866):
  * JSON responses on the web `/api/v1/` mount are gzipped when the client accepts it; a 155-agent list drops from 154 KB to about 18 KB. Streams are never compressed.
  * Static assets are served precompressed (brotli, then gzip) with strong per-variant ETags and 304 answers.
  * The terminal workspace loads on demand, shrinking `main.js` from 370 KB to 217 KB raw. CI enforces bundle and compression budgets.
  * Builds no longer carry stale chunks from earlier builds into the embedded Hub assets.
  * Agents and projects lists use keyed rows with stable identity; log viewers insert lines in place and batch rendering per frame; agent detail re-renders only for its own agent; live-event refetches in chat and notifications are coalesced; render-only timers pause in hidden tabs; long chat threads memoize per-message inputs.
* **User access tokens for user and group administration** (#3838, #3816, #3783): Tokens can update another user's status (`user:suspend`) or profile (`user:update`), and administer groups with the group selectors, provided the group carries no authority beyond the caller's. Updating or resuming a schedule re-records the caller's attribution and frozen ceiling.
* **Runtime Broker state directory** (#3762): New `server.broker.state_dir` setting (`SCION_SERVER_RUNTIMEBROKER_STATEDIR`) sets the base for the launch journal, dispatch attempts and pending env-gather. Pointing it at a persistent volume lets a Runtime Broker recover in-flight launches after a restart. A relative path refuses startup.
* **Test Hubs** (#3791, #3848): A test runner can issue test admin identities through an admin workload issuer delegation (`issue-admin`, agent mode only). `scion server test-infra mark` refuses when the Hub ID is unpinned or `--hub-id` does not match.
* **Conduit, experimental** (#3733, #3820, #3841, #3847, #3857): Behind `hub.conduit`, tunnels, PTY and port proxy route across relays over an authenticated relay-peer bridge. Superseded Runtime Broker sessions close with 4409. Under the new `server.hub.conduit.target_stream_deadlines` setting (default off), the Hub renews the deadlines of user streams it holds, and relay-held and Hub-held re-checks share one evaluation.
* **Groundwork** (#3819): Project visitors are admitted only on the routes and methods listed for them, answering a uniform 403 otherwise (behind `federation.project_visitors`, default off).

## 🔒 Security
* **No compression on sensitive responses** (#3865): Authorization catalog entries declare when a response carries credentials, signed object-store URLs or artifact capabilities, and a test fails if any such route would be gzip-compressed.
* **Sanitized create failure logs** (#3850): Logged create and async launch failure causes blank values and credential-shaped strings and are truncated. Client responses are unchanged.

## 🐛 Fixes
* **Kubernetes launch and preemption** (#3854, #3844): A Pending pod is reported as starting, not running, so async launches no longer end before the container starts. A gracefully preempted or evicted agent now ends `halted` instead of stopped, with a new `signaled` exit reason.
* **Cloud Run sandbox delete** (#3719): Hub delete of a running agent now removes its sandbox instead of reporting success and leaving it behind.
* **Shared-checkout worktrees** (#3823): Worktree removal and lookup in the shared checkout remove only the agent's own entry, so other agents' worktrees keep their git registration.
* **Safer `--purge-local`** (#3842): `deregister --purge-local` checks every Runtime Broker local entry against its expected location, refusing symlinked or out-of-root paths, before removing any.
* **Templates and config** (#3810, #3807, #3751, #3795): Template import failures are returned, native templates survive a forced self-import, imported template names are validated before writing, and harness detection matches full directory names. *(Contributor: Ravshanbek Nazarov)*
* **Utilities** (#3812, #3813, #3745): API client retries stop on context cancellation, UTF-8 text is normalized across the sample boundary in transfers, and non-finite CPU quantities are rejected. *(Contributor: Ravshanbek Nazarov)*
* **`whoami` output** (#3674): The slug and JSON output go to the command's output stream. *(Contributor: Ruslan Semagin)*

## 📖 Docs
* **Settings precedence** (#3784): Stale local-mode qualifiers are dropped from the image, profile and template ladders after local-mode removal.
