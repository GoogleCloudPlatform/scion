# Release Notes (2026-07-22)

Multi-scope skill auto-injection shipped with CLI commands for project and user skill management, relative workspace paths landed end-to-end across CLI/hub/dispatcher/broker, GKE hosted broker dispatch received another round of fixes, and the agent/project list limit was raised from 50 to 500 with cursor pagination fixes.

## 🚀 Features
* **[Skills]:** Multi-scope skill auto-injection — new `project_skill` and `user_skill` ent schemas, CLI commands for managing project and user skills, automatic injection into agent provisioning based on scope resolution (#846).
* **[Agent]:** Relative `--workspace` paths scoped to project logical root — resolves subdirectory paths against project root with containment checks (traversal, symlink escape), preserves relative paths through the hub/dispatcher pipeline (#815).
* **[Discord]:** Autocomplete agent parameter on `/default` command for large projects with case-insensitive slug validation (#844).
* **[Claude Harness]:** Add deny list and disable flags to `settings.json` (#847).

## 🐛 Fixes
* **[Hub]:** Raise agent/project list limit from 50 to 500 and fix agent cursor pagination — adds design doc, CLI/hub/store tests, and web UI pagination support (#848).
* **[Broker]:** GKE hosted broker dispatch fixes — K8s runtime improvements, plugin hub client adjustments, and Discord/Telegram broker registration updates (#842).
* **[Broker]:** Wire installed/restarted broker plugin as FanOut spoke — without spoke wiring, plugins never received `Subscribe()` calls so `startGateway()` never fired until full hub restart (#845).
* **[Discord]:** Default state notifications to off for new channel links (#835).

## 🔧 Chores
* **[Deps]:** Bump sharp 0.34.5→0.35.3, google.golang.org/grpc, fast-uri 3.1.2→3.1.4, brace-expansion 1.1.12→1.1.16, dompurify 3.4.11→3.4.12, svgo 4.0.1→4.0.2 (#836, #837, #838, #839, #840, #841).
