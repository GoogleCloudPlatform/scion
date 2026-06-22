# Release Notes (2026-06-21)

Antigravity auth was iterated on significantly — gnome-keyring was restored after the previous removal broke flows, token paths were fixed for bind-mounted secrets, and GCP settings detection was patched. A lifecycle hooks integration with Google Cloud Agent Registry was demonstrated, and the build system learned to use config.yaml image names.

## 🚀 Features
* **[Antigravity]:** Restored gnome-keyring to the provision flow — AGY requires keyring initialization before writing the OAuth token file, so keyring packages, DBUS initialization, and `secret-tool` injection were added back while keeping the `AGY_TOKEN` rename (#461).
* **[Lifecycle Hooks]:** Added `PROJECT_SLUG` as a trusted lifecycle hook variable and demonstrated Google Cloud Agent Registry integration — hooks POST an A2A agent card on agent start and DELETE the registration on stop. Includes integration test and docs example. Also fixed `VerificationStatus` derivation in the ent adapter (#demo).
* **[Skills]:** Hand-tuned team-builder skill content.

## 🐛 Fixes
* **[Antigravity]:** Read `AGY_TOKEN` from bind-mounted target path (`~/.gemini/antigravity-cli/antigravity-oauth-token`) instead of the env secret staging directory, fixing token detection in `_select_auth_method`, `_provision`, and `agy-wrapper.sh` (#465, #469).
* **[Antigravity]:** Patch GCP settings block for oauth-token agents when `GOOGLE_CLOUD_PROJECT` is present in the container environment, fixing the gate that only triggered on the enterprise marker file or `AGY_USE_GCP` env var (#462).
* **[Build]:** Use `config.yaml` image field to determine output image name instead of always using the harness-config CLI argument, ensuring the built image matches the intended name from the config (#463, #464, #468).
