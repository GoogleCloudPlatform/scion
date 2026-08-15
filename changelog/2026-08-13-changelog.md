# Release Notes (2026-08-13)

Native web chat Wave 2 landed with project-scoped spaces, threads, DMs, and presence, while a sweep of infrastructure fixes addressed eventbus fanout bugs, plugin hot-reconfigure failures, harness build/naming inconsistencies, and CI lint filtering.

## 🚀 Features
* **[Web Chat]:** Native chat Wave 2 — project-scoped spaces with shared threads, human-to-human and human-to-agent DMs (global pair), members sidebar with presence and typing indicators, composer default-agent disambiguation, attachments, search, and notifications. 10 phases, all reviewed (#1170).
* **[Hub]:** Wire DB-backed runtimes and profiles to broker consumption path — completes the Layer-1 opsettings registration from #1145 by connecting persisted settings to runtime agent dispatch (#1167).

## 🐛 Fixes
* **[Hub]:** Resolve three FanOutEventBus bugs — nil message dereference before guard check, silent message loss when channel-targeted publish to missing spoke returned error before inproc delivery, and handler contract violation where Subscribe fanned the same handler to every spoke causing double delivery risk. Includes 4 new regression tests (#1166).
* **[Plugin]:** Restart plugin process on hot-reconfigure instead of in-process config push — fixes stale go-plugin MuxBroker host callbacks that caused broker plugins installed at runtime to fail subscribing after initial handshake with empty config (#1169).
* **[Hub]:** Fix CloudRunInstances nested struct koanf unmarshal — `ProjectID` was always empty because koanf silently drops keys in nested structs within maps, causing 500 on agent dispatch (#1168).
* **[Harness]:** Fix gemini-cli build config coverage and image naming — add gemini-cli to both cloudbuild configs, fix image name `scion-gemini` → `scion-gemini-cli`, add copilot + hermes to imagepull map, remove orphaned Dockerfile (#1175). Map dialect name to harness ID at templateimport boundary — imported Gemini templates produced `default-harness-config: gemini` (invalid slug) instead of `gemini-cli` (#1177).
* **[CI]:** Replace `only-new-issues` with git-based `--new-from-merge-base=origin/main` lint filtering — GitHub's API refuses diffs over 20K lines, causing golangci-lint to fall back to a full-codebase scan that surfaced pre-existing QF1001 as a CI blocker (#1174). Apply De Morgan's law to the offending expression in oauth.go (#1171).
* **[Hub]:** Add `--no-block` variant to starter-hub sudoers template — maintenance restart handler uses `systemctl restart --no-block` but sudoers performed exact command matching (#1164).
* **[Web]:** Migrate three manual `pushState` + `PopStateEvent` navigation patterns to `navigateTo()` for consistent base-path handling behind reverse proxies (#1165).

## 🧪 Tests
* **[Harness]:** Add `TestAllHarnessNames_MatchesDisk` guard test — reads the real `harnesses/` directory and asserts it matches `AllHarnessNames()` from the embedded FS, making the hand-maintained `//go:embed` directive self-policing (#1176).

## 📖 Docs
* **[Docs]:** Nightly documentation update for Aug 12 — native web chat, Teams default agent command, JWKS/OIDC proxy, admin Runtimes & Profiles tab, workspace storage, Layer-1 settings, dashboard chat mode (#1173).
