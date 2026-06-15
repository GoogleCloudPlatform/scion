# Release Notes (2026-06-14)

The Skill Bank gained a full web UI, the Discord bot received significant fixes, and harness provisioning was hardened for no-auth mode.

## 🚀 Features
* **[Web — Skill Bank UI]:** Complete web interface for skill management — list page with search/scope filtering, detail page with version history and metadata, create page with scaffolding, and a publish dialog for uploading skills to the Hub. Admin pages for skill registry management (list, detail, CRUD). Added 4,200+ lines of Lit components across 7 new pages (#423).
* **[Web]:** Auto-link chat accounts when registration link includes `?code=` parameter — shows a clean "Linking... → Success!" flow instead of the manual code-entry form (#426).

## 🐛 Fixes
* **[Discord]:** Multiple fixes to the Discord bot — improved broker message handling, command routing, send queue reliability, and webhook delivery (422 lines changed across broker, commands, sendqueue, and webhooks) (#428).
* **[Harness]:** Handle no-auth mode in container-script provisioners, preventing auth-related failures when agents are configured without authentication (#424).
* **[Build]:** Added missing `!no_sqlite` build tags to test files depending on `messagebroker_test.go`, fixing `go vet -tags no_sqlite` failures (#264).

## 🔧 Chores
* **[Style]:** Minor `gofmt` formatting fixes (#264).
