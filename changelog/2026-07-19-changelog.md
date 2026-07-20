# Release Notes (2026-07-19)

GKE hosted broker dispatch received two rounds of fixes for hub endpoint derivation and IAP transport auth, @mention routing was corrected to use TypeMention instead of group recipients with a fallback-to-default guard, and Discord observed messages now display under the correct sender webhook identity.

## 🐛 Fixes
* **[Hub]:** GKE hosted broker dispatch — derive hub endpoint from IAP audience URL instead of hardcoding, fix broker auth token flow, and correct IAP transport audience configuration (#810).
* **[Hub]:** Follow-up GKE dispatch fix — decouple transport `oidc_audience` from hub endpoint so IAP-protected hubs resolve the audience independently (#814).
* **[Discord/Telegram]:** Route body @mentions as `TypeMention` messages instead of injecting mentioned agents as group recipients — fixes incorrect multi-agent dispatch on mention (#811).
* **[Discord/Telegram]:** Restore default agent target when body-mention filtering empties the target list, with guards against human-mention and slash-command messages (#812).
* **[Discord]:** Show observed messages under the actual sender's webhook identity and avatar instead of the topic agent's — adds gray-sidebar embed styling to distinguish relayed messages (#813).

## 🔧 Chores
* **[Skills]:** Updated built-in skills content.
