// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hub

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/keyring"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Key status (ptone/scion#1530 Phase 6, T0): one read-only, versioned
// report of a Hub instance's server key configuration. The admin endpoint
// GET /api/v1/admin/keys/status builds it from the serving process's own
// loaded keyring, cipher, store and startup audit; `scion server keys
// status` builds it from the locally loaded keyring file and store. Both
// use buildKeyStatus, so for the same file and store they report the same
// fields (instance identity aside). It is also the per-replica preparation
// report activate-signing consumes: each replica reports its own loaded
// configuration, never an operator assertion.
//
// The report holds IDs, formats, deadlines, policy and migration state
// (the migration ID included), aggregate counts and readiness reasons. It
// never holds key bytes, derived keys, ciphertext, payloads or secret
// record identifiers.

// KeyStatusReportVersion is the report_version of KeyStatusReport. A
// consumer refuses a version it does not know.
const KeyStatusReportVersion = 1

// KeyStatusInstanceSource values: who built the report.
const (
	// KeyStatusSourceEndpoint: the Hub endpoint, from its own process.
	KeyStatusSourceEndpoint = "endpoint"
	// KeyStatusSourceLocal: the CLI, from the local file and store.
	KeyStatusSourceLocal = "local"
)

// MaterialDigestOmittedPendingLegacyIndependence is the
// material_digest_omitted_reason of every report today. A whole-keyring
// digest is safe to publish only when the keyring always holds a random
// 32-byte encryption key. In legacy mode, and in every keyring file this
// release accepts (keyring.Keyring.CheckSessionPilot), the active
// encryption key is derived from the legacy signing secret, so a digest
// would be an offline oracle for a weak legacy secret. The digest is
// omitted until the legacy signing-secret dependence is removed (P6 T7/T10,
// sec-review-3 ruling on Q3).
const MaterialDigestOmittedPendingLegacyIndependence = "pending_legacy_independence"

// readyReasonEncryptionPolicyMismatch is the key status readiness reason
// of an instance whose cipher the database encryption policy refuses
// (checkEncryptionPolicy). A serving Hub refuses to start on it; the CLI
// reports it for the file it loaded.
const readyReasonEncryptionPolicyMismatch = "encryption_policy_mismatch"

// keyStatusSigningReaderFormats and keyStatusEncryptionReaderFormats are
// the formats this binary reads, whatever the keyring emits or writes.
var (
	keyStatusSigningReaderFormats    = []string{string(keyring.EmitLegacy), string(keyring.EmitVersioned)}
	keyStatusEncryptionReaderFormats = []string{string(keyring.WriteV1), string(keyring.WriteV2)}
)

// KeyStatusReport is the key status report, version 1. Every field is
// always present; an absent value is JSON null, never an omitted field.
type KeyStatusReport struct {
	ReportVersion int                    `json:"report_version"`
	Instance      KeyStatusInstance      `json:"instance"`
	Keyring       KeyStatusKeyring       `json:"keyring"`
	Signing       KeyStatusSigning       `json:"signing"`
	Encryption    KeyStatusEncryption    `json:"encryption"`
	ReaderFormats KeyStatusReaderFormats `json:"reader_formats"`
	Policy        *KeyStatusPolicy       `json:"policy"`
	Migration     *KeyStatusMigration    `json:"migration"`
	Audit         KeyStatusAudit         `json:"audit"`
	Readiness     KeyStatusReadiness     `json:"readiness"`
	// MaterialDigest is always null today; MaterialDigestOmittedReason
	// says why (MaterialDigestOmittedPendingLegacyIndependence).
	MaterialDigest              *string `json:"material_digest"`
	MaterialDigestOmittedReason string  `json:"material_digest_omitted_reason"`
}

// KeyStatusInstance identifies the reporting instance (non-secret).
type KeyStatusInstance struct {
	// ID is the Hub instance ID (Server.InstanceID); "" for a local report.
	ID       string `json:"id"`
	Hostname string `json:"hostname"`
	// Source is KeyStatusSourceEndpoint or KeyStatusSourceLocal.
	Source string `json:"source"`
}

// KeyStatusKeyring says where the keyring came from.
type KeyStatusKeyring struct {
	// Source is "file" (server.keyring_file) or "legacy" (the adapter over
	// the single legacy server secret).
	Source string `json:"source"`
	// Revision is the file's delivery revision; null in legacy mode.
	Revision *string `json:"revision"`
}

// KeyStatusSigning describes the signing ring.
type KeyStatusSigning struct {
	ActiveID string `json:"active_id"`
	// AcceptedIDs lists every generation, active first, then file order.
	AcceptedIDs   []string `json:"accepted_ids"`
	Emit          string   `json:"emit"`
	LegacyReadIDs []string `json:"legacy_read_ids"`
	// Generations has one entry per accepted ID, in the same order.
	Generations []KeyStatusGeneration `json:"generations"`
}

// KeyStatusGeneration is one signing generation's deadlines; null means
// no bound.
type KeyStatusGeneration struct {
	ID          string     `json:"id"`
	SignUntil   *time.Time `json:"sign_until"`
	VerifyUntil *time.Time `json:"verify_until"`
}

// KeyStatusEncryption describes the encryption ring.
type KeyStatusEncryption struct {
	ActiveID        string   `json:"active_id"`
	AcceptedIDs     []string `json:"accepted_ids"`
	WriteFormat     string   `json:"write_format"`
	LegacyV1ReadIDs []string `json:"legacy_v1_read_ids"`
}

// KeyStatusReaderFormats lists the wire formats this binary reads.
type KeyStatusReaderFormats struct {
	Signing    []string `json:"signing"`
	Encryption []string `json:"encryption"`
}

// KeyStatusPolicy is the database encryption policy; the report's Policy
// is null when none exists.
type KeyStatusPolicy struct {
	Generation       int64    `json:"generation"`
	State            string   `json:"state"`
	MinReaderVersion int      `json:"min_reader_version"`
	AllowedWriteKIDs []string `json:"allowed_write_kids"`
	RequiredReadKIDs []string `json:"required_read_kids"`
}

// KeyStatusMigration is the migration the policy binds; the report's
// Migration is null when the policy binds none.
type KeyStatusMigration struct {
	ID        string `json:"id"`
	State     string `json:"state"`
	TargetKID string `json:"target_kid"`
}

// KeyStatus audit scopes: which audit the report's counts come from.
const (
	// KeyStatusAuditStartup: the endpoint's startup audit, run in New
	// before any backup write; rows written later are not in it.
	KeyStatusAuditStartup = "startup"
	// KeyStatusAuditFresh: an audit the CLI ran for this report.
	KeyStatusAuditFresh = "fresh"
)

// KeyStatusAudit is the encryption audit aggregate: counts only.
type KeyStatusAudit struct {
	// Ran is false when no audit ran (no store).
	Ran bool `json:"ran"`
	// Scope is KeyStatusAuditStartup or KeyStatusAuditFresh.
	Scope string `json:"scope"`
	// CompletedAt is when the audit finished; null when none ran.
	CompletedAt   *time.Time `json:"completed_at"`
	Scanned       int        `json:"scanned"`
	ReferenceOnly int        `json:"reference_only"`
	Readable      int        `json:"readable"`
	Unreadable    int        `json:"unreadable"`
	// ByFormat counts payloads by envelope format (plaintext, v1, v2,
	// invalid); ByKID counts v2 payloads by envelope key ID.
	ByFormat map[string]int `json:"by_format"`
	ByKID    map[string]int `json:"by_kid"`
	// UnreadableByKind counts unreadable records by failure kind.
	UnreadableByKind map[string]int `json:"unreadable_by_kind"`
	// BackupUnreadable counts unreadable infrastructure backups.
	BackupUnreadable int `json:"backup_unreadable"`
}

// KeyStatusReadiness lists the key-related not-ready reasons (signing
// issuance, unreadable backups, encryption policy); an empty list means
// none. It is not the whole /readyz answer.
type KeyStatusReadiness struct {
	Reasons []string `json:"reasons"`
}

// keyStatusIDPurpose derives nothing: the report reads IDs and deadlines
// only, never key bytes.
var keyStatusIDPurpose = keyring.NewPurpose("key_status_ids", func([]byte) []byte { return nil })

// keyStatusInputs is everything buildKeyStatus reads.
type keyStatusInputs struct {
	instance KeyStatusInstance
	// keyring is the loaded keyring; nil means legacy mode.
	keyring *keyring.Keyring
	// cipher is the instance's ValueCipher (for the policy check).
	cipher *secret.ValueCipher
	// store is nil when no store is available.
	store store.Store
	audit EncryptionAudit
	// auditScope is KeyStatusAuditStartup or KeyStatusAuditFresh.
	auditScope string
	now        time.Time
}

// buildKeyStatus builds the report. It returns an error only when the
// store cannot be read; the error carries no key material.
func buildKeyStatus(ctx context.Context, in keyStatusInputs) (KeyStatusReport, error) {
	k := in.keyring
	if k == nil {
		// Legacy mode: the adapter's shape (IDs, emit, formats, no
		// deadlines) with no secret; only IDs are read from it.
		k = keyring.Legacy("")
	}
	r := KeyStatusReport{
		ReportVersion:               KeyStatusReportVersion,
		Instance:                    in.instance,
		Keyring:                     keyStatusKeyring(k),
		Signing:                     keyStatusSigning(k.Signing()),
		Encryption:                  keyStatusEncryption(k.Encryption()),
		ReaderFormats:               KeyStatusReaderFormats{Signing: slices.Clone(keyStatusSigningReaderFormats), Encryption: slices.Clone(keyStatusEncryptionReaderFormats)},
		Audit:                       keyStatusAudit(in.audit, in.auditScope),
		MaterialDigest:              nil,
		MaterialDigestOmittedReason: MaterialDigestOmittedPendingLegacyIndependence,
	}
	// The same conditions as /readyz: signingNotReadyReason checks any
	// keyring but the legacy adapter, backupNotReadyReason a managed cipher.
	var reasons []string
	if in.keyring != nil && in.keyring.Source() != keyring.SourceLegacy {
		if reason := signingIssuanceReason(k.Signing(), in.now); reason != "" {
			reasons = append(reasons, reason)
		}
	}
	if in.cipher.Managed() && len(in.audit.BackupUnreadable) > 0 {
		reasons = append(reasons, readyReasonBackupUnreadable)
	}
	if in.store != nil {
		policy, migration, err := keyStatusPolicy(ctx, in.store)
		if err != nil {
			return KeyStatusReport{}, err
		}
		r.Policy, r.Migration = policy, migration
		if err := checkEncryptionPolicy(ctx, in.store, in.cipher); err != nil {
			if !errors.Is(err, store.ErrEncryptionPolicyMismatch) {
				return KeyStatusReport{}, err
			}
			reasons = append(reasons, readyReasonEncryptionPolicyMismatch)
		}
	}
	if reasons == nil {
		reasons = []string{}
	}
	r.Readiness = KeyStatusReadiness{Reasons: reasons}
	return r, nil
}

func keyStatusKeyring(k *keyring.Keyring) KeyStatusKeyring {
	out := KeyStatusKeyring{Source: string(k.Source())}
	if k.Source() == keyring.SourceFile {
		rev := k.Revision()
		out.Revision = &rev
	}
	return out
}

func keyStatusSigning(r *keyring.SigningRing) KeyStatusSigning {
	out := KeyStatusSigning{
		ActiveID:      r.ActiveID(),
		AcceptedIDs:   nonNil(r.IDs()),
		Emit:          string(r.Emit()),
		LegacyReadIDs: []string{},
		Generations:   []KeyStatusGeneration{},
	}
	// The zero time is before every deadline, so this lists every
	// configured legacy reader, in file order.
	for _, key := range r.Candidates(keyStatusIDPurpose, "", time.Time{}) {
		out.LegacyReadIDs = append(out.LegacyReadIDs, key.ID)
	}
	for _, key := range r.Generations(keyStatusIDPurpose) {
		out.Generations = append(out.Generations, KeyStatusGeneration{
			ID: key.ID, SignUntil: key.SignUntil, VerifyUntil: key.VerifyUntil,
		})
	}
	return out
}

func keyStatusEncryption(r *keyring.EncryptionRing) KeyStatusEncryption {
	out := KeyStatusEncryption{
		ActiveID:        r.ActiveID(),
		AcceptedIDs:     nonNil(r.IDs()),
		WriteFormat:     string(r.WriteFormat()),
		LegacyV1ReadIDs: []string{},
	}
	for _, key := range r.Candidates("") {
		out.LegacyV1ReadIDs = append(out.LegacyV1ReadIDs, key.ID)
	}
	return out
}

func keyStatusAudit(a EncryptionAudit, scope string) KeyStatusAudit {
	out := KeyStatusAudit{
		Ran:              a.Done,
		Scope:            scope,
		Scanned:          a.Scanned,
		ReferenceOnly:    a.ReferenceOnly,
		Readable:         a.Readable,
		Unreadable:       len(a.Unreadable),
		ByFormat:         map[string]int{},
		ByKID:            map[string]int{},
		UnreadableByKind: map[string]int{},
		BackupUnreadable: len(a.BackupUnreadable),
	}
	if a.Done && !a.At.IsZero() {
		at := a.At.UTC()
		out.CompletedAt = &at
	}
	for f, n := range a.ByFormat {
		out.ByFormat[f] = n
	}
	for kid, n := range a.ByKID {
		out.ByKID[kid] = n
	}
	for _, rec := range a.Unreadable {
		out.UnreadableByKind[rec.Kind]++
	}
	return out
}

// keyStatusPolicy reads the database encryption policy and the migration
// it binds; both are nil when absent.
func keyStatusPolicy(ctx context.Context, st store.SecretEncryptionStore) (*KeyStatusPolicy, *KeyStatusMigration, error) {
	p, err := st.GetEncryptionPolicy(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("reading the database encryption policy: %w", err)
	}
	policy := &KeyStatusPolicy{
		Generation:       p.Generation,
		State:            string(p.State),
		MinReaderVersion: p.MinReaderVersion,
		AllowedWriteKIDs: nonNil(slices.Clone(p.AllowedWriteKIDs)),
		RequiredReadKIDs: nonNil(slices.Clone(p.RequiredReadKIDs)),
	}
	if p.MigrationID == "" {
		return policy, nil, nil
	}
	m, err := st.GetEncryptionMigration(ctx, p.MigrationID)
	if errors.Is(err, store.ErrNotFound) {
		// A dangling binding: report the ID with no state.
		return policy, &KeyStatusMigration{ID: p.MigrationID}, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("reading the encryption migration: %w", err)
	}
	return policy, &KeyStatusMigration{ID: m.ID, State: string(m.State), TargetKID: m.TargetKID}, nil
}

// signingIssuanceReason is the /readyz signing reason of a keyring file's
// signing ring at now (Server.signingNotReadyReason, without its log
// line): "" while the active generation can issue.
func signingIssuanceReason(r *keyring.SigningRing, now time.Time) string {
	_, err := r.Active(keyStatusIDPurpose, now)
	switch {
	case err == nil:
		return ""
	case errors.Is(err, keyring.ErrIssuanceClosed):
		return readyReasonSigningIssuanceClosed
	default:
		return readyReasonSigningNoActiveGeneration
	}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// KeyStatus builds this instance's key status report from its own loaded
// keyring, cipher, store and startup audit.
func (s *Server) KeyStatus(ctx context.Context) (KeyStatusReport, error) {
	host, _ := os.Hostname()
	return buildKeyStatus(ctx, keyStatusInputs{
		instance:   KeyStatusInstance{ID: s.InstanceID(), Hostname: host, Source: KeyStatusSourceEndpoint},
		keyring:    s.config.Keyring,
		cipher:     s.cipher,
		store:      s.store,
		audit:      s.EncryptionAudit(),
		auditScope: KeyStatusAuditStartup,
		now:        s.keyringNow(),
	})
}

// LocalKeyStatus builds the key status report for a locally loaded keyring
// (nil: legacy mode) and its cipher. With a store it reads the encryption
// policy and runs the same authenticated audit a starting Hub runs; st may
// be nil. In legacy mode without a session secret the cipher is disabled,
// as it is in a Hub started without one; the audit still runs, exactly as
// that Hub's startup audit does, so the two reports agree. It never writes
// to the store.
func LocalKeyStatus(ctx context.Context, k *keyring.Keyring, c *secret.ValueCipher, st store.Store, now time.Time) (KeyStatusReport, error) {
	if k != nil && k.Source() != keyring.SourceFile {
		k = nil
	}
	audit := EncryptionAudit{}
	if st != nil {
		a, err := runEncryptionAudit(ctx, st, c)
		if err != nil {
			return KeyStatusReport{}, err
		}
		audit = a
	}
	host, _ := os.Hostname()
	return buildKeyStatus(ctx, keyStatusInputs{
		instance:   KeyStatusInstance{Hostname: host, Source: KeyStatusSourceLocal},
		keyring:    k,
		cipher:     c,
		store:      st,
		audit:      audit,
		auditScope: KeyStatusAuditFresh,
		now:        now,
	})
}

// CheckKeyringFile applies the keyring-file checks a starting Hub applies
// to a loaded file beyond the loader's own validation: the "legacy" signing
// entry, this release's limits (keyringReleaseCheck) and, with emit
// "legacy", the active generation as a legacy reader. Errors name no key
// material.
func CheckKeyringFile(k *keyring.Keyring) error {
	if k == nil || k.Source() != keyring.SourceFile {
		return errors.New("keyring file: not loaded from a keyring file")
	}
	if _, ok := k.Signing().LegacySecret(); !ok {
		return errors.New("keyring mode: the signing ring has no \"legacy\" entry; every purpose that does not use the ring yet still needs it")
	}
	if err := keyringReleaseCheck(k); err != nil {
		return err
	}
	return checkLegacyEmitActiveReader(k.Signing())
}

// handleAdminKeyStatus serves GET /api/v1/admin/keys/status. The route
// guard admits unscoped platform admins only (no delegable permission);
// requireExperiment answers 404 while server.secret_rotation is off.
func (s *Server) handleAdminKeyStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}
	report, err := s.KeyStatus(r.Context())
	if err != nil {
		slog.Error("Key status report failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, ErrCodeInternalError, "key status unavailable", nil)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, report)
}

// adminKeyStatusHandler is the experiment-gated key status handler the
// route registers.
func (s *Server) adminKeyStatusHandler() http.HandlerFunc {
	return s.requireExperiment(experiments.SecretRotation, s.handleAdminKeyStatus)
}

// Material digest (not wired into the report; see
// MaterialDigestOmittedPendingLegacyIndependence).

// materialDigestDomain is the label the digest input starts with (P6 plan
// r2 erratum E1); it separates the digest from every other hash of the
// same bytes.
const materialDigestDomain = "scion-keyring-material-v1\n"

// materialDigestSecretPurpose hands each signing generation's secret to
// keyringMaterialDigest, which hashes it at once.
var materialDigestSecretPurpose = keyring.NewPurpose("material_digest", func(s []byte) []byte { return slices.Clone(s) })

// keyringMaterialDigest is SHA-256 over the label materialDigestDomain
// followed by a length-prefixed canonical serialization of the whole parsed
// keyring (P6 plan r2 erratum E1): source, revision, both
// rings' active ID, emit or write format, every entry's ID and material
// (signing deadlines included) and the legacy reader lists. It is the
// recommended construction for fleet material equality; it must not be
// published while any reported keyring state can lack a random encryption
// key.
func keyringMaterialDigest(k *keyring.Keyring) string {
	return materialDigestWithDomain(materialDigestDomain, k)
}

// materialDigestWithDomain is keyringMaterialDigest under domain (a
// parameter so a test can prove the domain separates the digest).
func materialDigestWithDomain(domain string, k *keyring.Keyring) string {
	h := sha256.New()
	field := func(b []byte) {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(b)))
		_, _ = h.Write(n[:])
		_, _ = h.Write(b)
	}
	str := func(s string) { field([]byte(s)) }
	strs := func(ss []string) {
		str(fmt.Sprintf("%d", len(ss)))
		for _, s := range ss {
			str(s)
		}
	}
	deadline := func(t *time.Time) {
		if t == nil {
			field(nil)
			return
		}
		str(t.UTC().Format(time.RFC3339Nano))
	}
	_, _ = h.Write([]byte(domain))
	str(string(k.Source()))
	str(k.Revision())

	sr := k.Signing()
	str("signing")
	str(sr.ActiveID())
	str(string(sr.Emit()))
	gens := sr.Generations(materialDigestSecretPurpose)
	str(fmt.Sprintf("%d", len(gens)))
	for _, g := range gens {
		str(g.ID)
		field(g.Bytes())
		deadline(g.SignUntil)
		deadline(g.VerifyUntil)
	}
	var legacyReads []string
	for _, key := range sr.Candidates(keyStatusIDPurpose, "", time.Time{}) {
		legacyReads = append(legacyReads, key.ID)
	}
	strs(legacyReads)

	er := k.Encryption()
	str("encryption")
	str(er.ActiveID())
	str(string(er.WriteFormat()))
	ids := er.IDs()
	str(fmt.Sprintf("%d", len(ids)))
	for _, id := range ids {
		str(id)
		for _, key := range er.Candidates(id) {
			field(key.Bytes())
		}
	}
	var v1Reads []string
	for _, key := range er.Candidates("") {
		v1Reads = append(v1Reads, key.ID)
	}
	strs(v1Reads)
	return hex.EncodeToString(h.Sum(nil))
}
