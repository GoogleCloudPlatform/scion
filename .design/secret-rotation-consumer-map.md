# Secret rotation consumer map (ptone/scion#1530 Phase 6, task T1)

Status: inventory at `4c60666` (ptone/scion main). It is pinned by
`TestRingModeLegacyEntryCensus` in `pkg/secret/callsite_audit_test.go`. Task
T10 re-runs it at its own head (acceptance criterion 1).

This document re-runs the consumer map of the base design ("Existing consumer
map") and its addendum §1 against the integration tip. It covers:

- every `ensureSigningKey` caller;
- every direct reader of `SharedSigningSecret`, `resolveSessionSecret`,
  `sessionSecretValue`, `keyringLegacySecret` and `LegacySecret`;
- session derivation;
- every encryption call site;
- the startup and secret-backend wiring.

Line numbers are at `4c60666`.

## Classes

Each consumer is in exactly one class. The census uses the same names.

| Class | Meaning |
|---|---|
| on-ring | In managed ring mode (a keyring file), every key the consumer uses comes from the signing or encryption ring. |
| independently keyed | The key material is generated independently and is never derived from the server secret. The reason is given. |
| legacy-only | Reached only in legacy mode (no keyring file), or the value is unused in ring mode, or the site is a test fixture or tool. |
| **ring-mode legacy-entry reader** | Reads the keyring `legacy` signing entry in ring mode, directly or through `SharedSigningSecret`/`resolveSessionSecret`/`sessionSecretValue`, which return that entry when a keyring file is loaded. **This list is task T7's exact scope.** |
| release gate | The release refusal that requires the `legacy` entry until task T10 lifts it. |

## Ring-mode legacy-entry readers (T7 scope)

These are census keys (`<name>@<file>:<function>`). Methods are keyed `(*T).Name`. `TestRingModeLegacyEntryReadersInConsumerMap` requires this section and the release-gate section to list exactly the census keys of their class, no more and no fewer. When T7 has removed them all, this section must hold a line reading exactly `ring-mode legacy-entry readers: none` (and only then).

**The root of the chain.** With a keyring file loaded, every cmd consumer below reads the `legacy` entry through these three functions:

1. `LegacySecret@cmd/server_keyring.go:keyringLegacySecret` returns the file's `legacy` signing entry.
2. `keyringLegacySecret@cmd/server_foreground.go:resolveSessionSecret` (:2599) returns it as the session secret.
3. `keyringLegacySecret@cmd/server_foreground.go:sessionSecretValue` (:2621) does the same.

**Startup configuration and validation:**

4. `resolveSessionSecret@cmd/server_foreground.go:buildHubServerConfig` sets `hub.ServerConfig.SharedSigningSecret` to the `legacy` entry (:2899). Its second reference (:2902) feeds `webSessionKeySource`, which ignores the value when a keyring file is loaded.
5. `SharedSigningSecret@pkg/hub/keyring.go:validateKeyringConfig` requires `SharedSigningSecret` to equal the `legacy` entry (:126).
6. `LegacySecret@pkg/hub/keyring.go:validateKeyringConfig` requires a `legacy` entry (:116). The plan moves 5 and 6 behind the `keyringReleaseCheck` seam in T7 and lifts them in T10.
7. `resolveSessionSecret@cmd/server_foreground.go:validateHostedBasic` (:1608) warns when the session secret is empty. Without a `legacy` entry it would warn in ring mode.
8. `resolveSessionSecret@cmd/server_foreground.go:validateHostedHAPreflight` (:1644) refuses an empty session secret. Without a `legacy` entry, hosted HA would **refuse** ring mode.
9. `SharedSigningSecret@cmd/server_foreground.go:initHubServer` (:3109) is a hosted warning on an empty `SharedSigningSecret`.

**Relay-peer auth.** The HMAC signer already comes from the ring (`SigningRing`, `relay.NewHMACPeerAuthFromRing`). Only the PeerSecret inequality check still uses the `legacy` entry, and it compares against that entry alone. Obl 6 requires a comparison against every generation.

10. `resolveSessionSecret@cmd/server_conduit.go:conduitPeerAuthOptions` (:453) sets `ConduitPeerAuthOptions.SharedSecret`. The downstream comparison is `pkg/conduit/relay/conduit_peerauth.go:149` (`NewConduitPeerAuth`).
11. `sessionSecretValue@cmd/server_conduit.go:externalPeerAuthOptions` (:743) does the same in external relay mode.
12. `sessionSecretValue@cmd/server_foreground.go:validateServerPreflight` (:1738) passes the value to `validateConduitPeerSecret`. Its comparison is at `cmd/server_conduit.go:483`.

**Test admin tier G7.** These refuse a non-empty `SharedSigningSecret`, which in ring mode is the `legacy` entry. The keyring source is also refused by `signing_keys_hub_local`, so the tier stays closed. T7 must show that the refusal still holds without the `legacy` entry.

13. `SharedSigningSecret@cmd/server_test_admin_gate.go:testModeInputsFor` (:369)
14. `SharedSigningSecret@cmd/server_test_admin_gate.go:resolveTestMode` (:292)
15. `SharedSigningSecret@pkg/hub/test_admin_gate.go:validateTestAdminGateConfig` (:194)

### Release gate (T10 scope, not T7)

- `LegacySecret@pkg/keyring/load.go:(*Keyring).CheckSessionPilot` (:495)
- `DeriveLegacyEncryptionKey@pkg/keyring/load.go:(*Keyring).CheckSessionPilot` (:499): the active encryption key must equal the key derived from the `legacy` entry.

### Corrections to the plan's T7 file list

p6-plan-r3 lists the following as known T7 files. At `4c60666` they are **legacy-only**, and T7 does not need to change them:

- `pkg/hub/conduit_admission.go:169`: `conduitAdmissionRing` returns the keyring's signing ring first (:166) and reads `SharedSigningSecret` only without a keyring file.
- `pkg/hub/server.go:3715`: `ensureSigningKeyWithSource` is reached only through `ensureSigningKey`. Every `ensureSigningKey` caller sits behind `keyringManaged()` or `managedSigning` (see below).
- `cmd/server_foreground.go:3837` (`initWebServer`): `WebServerConfig.SessionSecret` is unused when `SigningRing` is set (`pkg/hub/web.go:733-745`).

The remaining items on the plan's list (`validateKeyringConfig`, :1608, :1644, :1738, :2899, `cmd/server_conduit.go:453,483,743` and `conduit_peerauth.go:149`) are confirmed above. The list also gains items the plan did not name: :3109, the G7 sites, and the three root resolvers.

## Signing consumers

| Consumer | Ring-mode source | Legacy-mode source | Class |
|---|---|---|---|
| Browser session cookies (OAuth state, Hub tokens) | `keyring.CookieCodecs` over `WebServerConfig.SigningRing` (`pkg/hub/web.go:733,778`; `cmd/server_foreground.go:3895`) | `keyring.Legacy(SessionSecret)` (`web.go:745`), or a random key | on-ring |
| Agent HS256 JWTs | `initRingTokenServices` (`pkg/hub/server.go:3641`), `agentJWTPurpose` (`jwt_keys.go:33`) | `ensureSigningKey` in `initLegacyTokenServices` (`server.go:3591`) | on-ring |
| User HS256 JWTs | `initRingTokenServices`, `userJWTPurpose` (`jwt_keys.go:36`) | `ensureSigningKey` (`server.go:3616`) | on-ring |
| Skill-file download URLs | `managedCapabilityKeys` (`download_signing.go:112`) | `ensureSigningKey` (`:120`) | on-ring |
| Artifact view capabilities | `managedCapabilityKeys` (`artifacts_store.go:268`) | `ensureSigningKey` (`:278`); `artifacts.LegacyViewKeys` | on-ring |
| Port view capabilities | `managedCapabilityKeys` (`port_view.go:78`) | `ensureSigningKey` (`:88`) | on-ring |
| Authorized-list sealed cursors | `managedCapabilityKeys` (`authorized_list.go:817`) | `ensureSigningKey` (`:825`) | on-ring |
| **Attachment delivery capabilities** | `managedCapabilityKeys` (`attachment_delivery.go:93`), versioned `ad2` tokens | `ensureSigningKey` (`:103`) | **on-ring (integrated)** |
| Conduit admission tokens | `conduitAdmissionRing` returns the keyring signing ring (`conduit_admission.go:165-167`) | `keyring.Legacy(SharedSigningSecret)` (`:169-173`) | on-ring |
| Relay-peer HMAC (no dedicated PeerSecret) | `NewHMACPeerAuthFromRing` (`conduit_peerauth.go:156`; `cmd/server_conduit.go:461`) | `NewHMACPeerAuthFromSecret` (`:161`) | on-ring; the PeerSecret check is a ring-mode legacy-entry reader (items 10-12) |
| Relay-peer HMAC with a dedicated PeerSecret | the PeerSecret | the PeerSecret | independently keyed: an operator-supplied separate secret with its own derivation |

Attachment delivery is covered by the P4 tests in `pkg/hub/capability_keyring_test.go`:

- `TestCapabilityKeyring_DeliveryCrossReplica`
- `TestCapabilityKeyring_DeliveryLegacyReplicaUpgrade`
- `TestCapabilityKeyring_DeliveryNonceSharedAcrossFormats`
- `TestCapabilityKeyring_DeliveryStrictFormatDoesNotConsumeNonce`
- `TestCapabilityKeyring_DeliveryRetirementBoundary`
- `TestCapabilityKeyring_DeliveryBindingAndLivenessRingMode`
- `TestCapabilityKeyring_DeliveryCrossReplicaBrokerHMAC`
- `TestCapabilityKeyring_DeliveryIssuanceClosed`

### `ensureSigningKey` callers (all legacy-only)

| Caller | Ring-mode branch |
|---|---|
| `pkg/hub/server.go:initLegacyTokenServices` (2 calls) | `managedSigning(cfg)` selects `initRingTokenServices` (`server.go:2832`) |
| `pkg/hub/download_signing.go:initDownloadSigningKey` | `keyringManaged()` |
| `pkg/hub/artifacts_store.go:initArtifactViewKey` | `keyringManaged()` |
| `pkg/hub/port_view.go:initPortViewKey` | `keyringManaged()` |
| `pkg/hub/authorized_list.go:initListCursorSealer` | `keyringManaged()` |
| `pkg/hub/attachment_delivery.go:initAttachmentDeliveryKey` | `keyringManaged()` |

`deriveSharedSigningKey` appears in three places:

- `capability_keys.go:sharedSecretPurpose` and `jwt_keys.go`: the ring purposes (on-ring);
- `server.go:ensureSigningKeyWithSource`: legacy-only;
- a copy in `perf/bench/seed`: a tool.

### Independently keyed (not derived from the server secret)

Each of these is classified in `tiaKeyMaterialSites` (`pkg/hub/test_admin_gate_test.go`).

| Consumer | Reason |
|---|---|
| OIDC RSA signing keys (`oidckeys.go`) | Generated RSA keys. Only their backups are encrypted (see Encryption). |
| Conduit Ed25519 grant ring (`conduit_grants.go:bootstrap,rotate`) | A random ring. Only its stored form is encrypted. |
| Broker join and per-broker HMAC (`brokerauth.go:computeHMAC`) | Random per broker. |
| Opaque user access keys (`useraccesstoken.go`) | Random and hash-backed. |
| Test-identity web session marker key (`handlers_test_identity_web.go`) | Generated at test-infra mark time. Hub-local. |
| Access-constraint preview tokens (`access_constraint_preview.go`) | Random per process, held in memory only. |
| GitHub App key and webhook secret (`githubapp/`) | External credentials. |

## Encryption consumers (all on-ring)

In ring mode, every stored Hub ciphertext goes through the one shared `secret.ValueCipher`, built from the keyring file's encryption ring. Pinned elsewhere:

- `TestNoDirectScalarCipherCallers`: no scalar helper callers;
- `TestDecryptForMigrationCallers`: empty allow-list.

| Call site | Purpose |
|---|---|
| `pkg/secret/localbackend.go:134` (Encrypt), `:476` (Decrypt) | Local-backend secret values (all scopes) |
| `pkg/hub/server.go:3807,3864` (Decrypt), `:4053` (Encrypt) | Hub signing-key backups (legacy-mode resolution and the backup re-save) |
| `pkg/hub/oidckeys.go:481,690,770` (Decrypt), `:577,644,735` (Encrypt) | OIDC key and keyset backups |
| `pkg/hub/conduit_grants.go:179` (Decrypt), `:198` (Encrypt) | Conduit grant ring |
| `pkg/hub/encryption_audit.go:223` (Inspect), `:231,362` (Decrypt) | Startup audit and the backup-readability check |

There is one plaintext path, `pkg/hub/hub_secrets.go:41` (`setHubSecret` with no secret backend, Obl 8). It writes no ciphertext and uses no key. It is T12's scope, not a legacy-entry reader.

### Cipher wiring

| Site | Ring mode | Class |
|---|---|---|
| `cmd/server_keyring.go:buildServerValueCipher` (:48) | `secret.NewValueCipher(keyring file)`. `NewLegacyValueCipher(resolveSessionSecret())` (:58) runs only without a file. | on-ring; the legacy branch is legacy-only |
| `cmd/server_foreground.go:503-512` | `buildServerValueCipher` runs before `secret.NewBackend(..., serverValueCipher)`, so the backend and the Hub share one object | on-ring |
| `cmd/server_foreground.go:2907` | `hub.ServerConfig.ValueCipher = serverValueCipher` | on-ring |
| `pkg/hub/keyring.go:serverValueCipher` (:142) | `cfg.ValueCipher` (which must wrap the file's ring), else the file. `NewLegacyValueCipher(SharedSigningSecret)` (:156) runs only without a file. | on-ring; the legacy branch is legacy-only |
| `pkg/secret/localbackend.go:NewLocalBackend` | A scalar-secret constructor. Production uses `NewBackend` with the shared cipher. | legacy-only |

## Census

`TestRingModeLegacyEntryCensus` scans every non-test Go file in the module, excluding `extras`, `web`, `docs-site` and `.scion`. It counts the references to the tracked names: `SharedSigningSecret`, `resolveSessionSecret`, `sessionSecretValue`, `keyringLegacySecret`, `LegacySecret`, `ensureSigningKey`, `deriveSharedSigningKey`, `NewLegacyValueCipher` and `DeriveLegacyEncryptionKey`.

Every `<name>@<file>:<function>` must be classified with its exact reference count. A reference is a selector, a call or a bare identifier used as a value (an alias or a callback); declared names and composite-literal keys are not references. A new reader, a new read inside a classified function, or a stale entry fails the test. `TestRingModeLegacyEntryCensusRefusesPlantedReader` is the negative case.

Not tracked by name:

- `SessionSecret` and `SharedSecret`. These generic field names are reached only through the tracked resolvers above.
- The `keyring.Legacy` adapter. Its callers are listed in the tables.
