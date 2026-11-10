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
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Delivery of attachments stored as artifacts into the recipient agent's
// home (ptone/scion#4596, design section 8).
//
// When the hub dispatches a message that carries bound artifact
// attachments to an agent, it plans each attachment once for that
// recipient (Plan, at the render site, so the envelope lists the inbox
// paths) and, at dispatch, issues one short-lived, single-use download
// capability per deliverable attachment (Issue, in the dispatcher, on every
// attempt). Both ask the one read decision for the recipient. The
// recipient's broker redeems the capabilities on
// GET /api/v1/attachment-deliveries/<token> with its own credential,
// verifies each file and writes it to ~/.scion/inbox/<message-id>/<name>
// before the message text is delivered.

const (
	// SecretKeyAttachmentDeliveryKey is the secret key name of the key that
	// signs attachment delivery capabilities. It is separate from every
	// other hub key.
	SecretKeyAttachmentDeliveryKey = "attachment_delivery_signing_key"

	// attachmentDeliveryTTL is how long a delivery capability stays valid.
	attachmentDeliveryTTL = 15 * time.Minute
	// attachmentDeliveryMaxSkew bounds how far beyond the TTL an expiry may
	// lie; the hub never issues one further out.
	attachmentDeliveryMaxSkew = time.Minute
	// attachmentDeliveryDomain separates the signed message from every
	// other signature of the hub.
	attachmentDeliveryDomain = "attachment-delivery"
	// attachmentDeliveryDomainV2 is the domain of the versioned token's
	// signed message (ptone/scion#1530 Phase 4).
	attachmentDeliveryDomainV2 = "attachment-delivery-v2"
	// attachmentDeliveryVersion2 is the first part of a versioned token:
	// "ad2.<kid>.<base64url(payload)>.<base64url(MAC)>".
	attachmentDeliveryVersion2 = "ad2"
	// attachmentDeliveriesPrefix is the redemption route.
	attachmentDeliveriesPrefix = "/api/v1/attachment-deliveries/"
	// attachmentInboxDir is where delivered attachments land in an agent's
	// home, one directory per message.
	attachmentInboxDir = "~/.scion/inbox/"
	// maxAttachmentNameRunes bounds a reduced attachment name.
	maxAttachmentNameRunes = messaging.MaxAttachmentNameRunes
	// deliveryRedemptionRetention is how long a redeemed nonce is kept;
	// longer than any capability lives.
	deliveryRedemptionRetention = 24 * time.Hour
)

// initAttachmentDeliveryKey loads or creates the attachment delivery
// signing key, with the same stable-key policy as the artifact view key:
// required on deployments that need stable keys, otherwise an in-memory key,
// since losing it only ends capabilities within attachmentDeliveryTTL.
//
// In keyring mode the keys come from the signing ring instead (see
// capability_keys.go) and nothing is read from the secret backend.
func (s *Server) initAttachmentDeliveryKey(ctx context.Context) error {
	if s.keyringManaged() {
		keys, err := s.managedCapabilityKeys(SecretKeyAttachmentDeliveryKey)
		if err != nil {
			return err
		}
		s.mu.Lock()
		s.attachmentDeliveryKeys = keys
		s.mu.Unlock()
		return nil
	}
	key, err := s.ensureSigningKey(ctx, SecretKeyAttachmentDeliveryKey, nil)
	if err == nil && len(key) == 0 {
		err = fmt.Errorf("attachment delivery key resolved to an empty value")
	}
	if err != nil {
		_, isGCPBackend := s.secretBackend.(*secret.GCPBackend)
		if isGCPBackend || s.config.RequireStableSigningKey {
			return fmt.Errorf("attachment delivery key: %w", err)
		}
		slog.Warn("Attachment delivery key could not be loaded or persisted; using an ephemeral in-memory key "+
			"(delivery capabilities will not redeem on other replicas or after restart)", "error", err)
		key = make([]byte, 32)
		if _, rerr := rand.Read(key); rerr != nil {
			return fmt.Errorf("generate ephemeral attachment delivery key: %w", rerr)
		}
		s.recordSigningKeySource(SecretKeyAttachmentDeliveryKey, signingKeySourceEphemeral)
	}
	s.mu.Lock()
	s.attachmentDeliveryKeys = fixedCapabilityKeys(key)
	s.mu.Unlock()
	return nil
}

// deliveryIssuanceClosedLogged makes the issuance-closed signing failure
// of Issue appear once per process.
var deliveryIssuanceClosedLogged atomic.Bool

// deliveryCapability is the signed content of one delivery capability: one
// attachment of one message, for one recipient agent on one broker, until
// Exp, once (Nonce).
type deliveryCapability struct {
	ArtifactID string `json:"a"`
	MessageID  string `json:"m"`
	AgentID    string `json:"g"`
	BrokerID   string `json:"b"`
	Exp        int64  `json:"e"`
	Nonce      string `json:"n"`
}

// deliveryMAC computes the HMAC-SHA256 under key of a capability's encoded
// payload, the exact bytes the token carries, behind a domain prefix. The
// MAC covers the bytes, not a re-encoding of the parsed fields, so a token
// has exactly one valid form.
func deliveryMAC(key, payload []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(attachmentDeliveryDomain + "\n"))
	m.Write(payload)
	return m.Sum(nil)
}

// deliveryMACv2 is deliveryMAC for a versioned token signed by generation
// kid: the message is "attachment-delivery-v2\n" || kid || "\n" || payload.
func deliveryMACv2(key []byte, kid string, payload []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(attachmentDeliveryDomainV2 + "\n" + kid + "\n"))
	m.Write(payload)
	return m.Sum(nil)
}

// deliveryKeys returns the delivery signing keys.
func (s *Server) deliveryKeys() capabilityKeys {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.attachmentDeliveryKeys
}

// signDeliveryCapability returns the token of c, signed by the active
// generation: base64url(JSON) "." base64url(MAC), or, when the signing ring
// emits versioned values, "ad2." kid "." base64url(JSON) "." base64url(MAC).
// It fails when no key is configured or none may sign now (the active
// generation reached its sign_until); the error names no key.
func (s *Server) signDeliveryCapability(c deliveryCapability) (string, error) {
	keys := s.deliveryKeys()
	if !keys.configured() {
		return "", errors.New("no attachment delivery key")
	}
	key, versioned, err := keys.issue()
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding.EncodeToString(payload)
	if versioned {
		return attachmentDeliveryVersion2 + "." + key.ID + "." + enc + "." +
			base64.RawURLEncoding.EncodeToString(deliveryMACv2(key.Bytes(), key.ID, payload)), nil
	}
	return enc + "." + base64.RawURLEncoding.EncodeToString(deliveryMAC(key.Bytes(), payload)), nil
}

// verifyDeliveryCapability returns the capability of token when its MAC
// verifies and it has not expired at now. Every failure answers false.
//
// Dispatch is exact: two parts are the legacy token, checked with the
// signing ring's legacy readers; four parts whose first is "ad2" are the
// versioned token, checked with generation kid only (an unknown or retired
// kid fails, never trying another key or the legacy form). The MAC is
// checked over the payload bytes before they are parsed.
func (s *Server) verifyDeliveryCapability(token string, now time.Time) (deliveryCapability, bool) {
	var c deliveryCapability
	keys := s.deliveryKeys()
	if !keys.configured() {
		return c, false
	}
	parts := strings.Split(token, ".")
	kid := ""
	switch {
	case len(parts) == 2:
	case len(parts) == 4 && parts[0] == attachmentDeliveryVersion2 && capabilityKeyIDPattern.MatchString(parts[1]):
		kid = parts[1]
		parts = parts[2:]
	default:
		return c, false
	}
	payloadPart, macPart := parts[0], parts[1]
	if payloadPart == "" || macPart == "" {
		return c, false
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(payloadPart)
	if err != nil {
		return c, false
	}
	got, err := base64.RawURLEncoding.Strict().DecodeString(macPart)
	if err != nil {
		return c, false
	}
	// The MAC is checked over the payload bytes before they are parsed.
	matched := false
	for _, k := range keys.candidates(kid) {
		var want []byte
		if kid == "" {
			want = deliveryMAC(k.Bytes(), payload)
		} else {
			want = deliveryMACv2(k.Bytes(), kid, payload)
		}
		if hmac.Equal(got, want) {
			matched = true
			break
		}
	}
	if !matched {
		return deliveryCapability{}, false
	}
	if err := json.Unmarshal(payload, &c); err != nil {
		return deliveryCapability{}, false
	}
	if c.ArtifactID == "" || c.MessageID == "" || c.AgentID == "" || c.BrokerID == "" || c.Nonce == "" {
		return deliveryCapability{}, false
	}
	nowUnix := now.Unix()
	if nowUnix > c.Exp || c.Exp > nowUnix+int64((attachmentDeliveryTTL+attachmentDeliveryMaxSkew)/time.Second) {
		return deliveryCapability{}, false
	}
	return c, true
}

// reduceAttachmentName reduces a stored attachment name to a name that is
// safe as a file name and in an envelope: [A-Za-z0-9._-] only (every other
// rune becomes '_'), at most maxAttachmentNameRunes runes, and never
// starting with '.' or '-' (such a name gets an "attachment-" prefix):
// messaging.ReduceAttachmentName, which the CLI shares.
func reduceAttachmentName(name string) string {
	return messaging.ReduceAttachmentName(name)
}

// attachmentInboxPath is where attachment name (reduced) of message
// messageID lands in the recipient's home.
func attachmentInboxPath(messageID, name string) string {
	return attachmentInboxDir + messageID + "/" + name
}

// attachmentDelivery plans and issues attachment deliveries. The render
// sites call Plan and the dispatcher calls Issue (as its AttachmentIssuer),
// so the envelope and the capabilities come from one decision.
type attachmentDelivery struct {
	s *Server
}

// attachmentDelivery returns the server's delivery component.
func (s *Server) attachmentDelivery() *attachmentDelivery {
	return &attachmentDelivery{s: s}
}

// attachmentDecisionForRecipient returns a context whose caller is the
// recipient agent, built from the hub's own agent row (like
// storedAgentIdentity on the broker retry path) on a fresh context, and a
// fresh standing memo. The context carries no credential record: only
// authentication middleware records one, and a stored agent identity is
// classified from its type as the agent credential kind. Only
// Plan and Issue build it: it is the one place the hub asks the read
// decision for someone other than the request's caller, and it does so only
// for a message the sender was authorized to send to that recipient.
func attachmentDecisionForRecipient(agent *store.Agent) context.Context {
	identity := &storedAgentIdentity{agent: agent}
	return withStandingMemo(contextWithIdentity(context.Background(), identity))
}

// recipientMayRead asks the shared read decision for the recipient of
// rctx (attachmentDecisionForRecipient) and the attachments of messageID,
// memoized per message in memo.
func (d *attachmentDelivery) recipientMayRead(rctx context.Context, messageID string, memo map[string]bool) bool {
	if v, ok := memo[messageID]; ok {
		return v
	}
	svc := d.s.attachmentService()
	found, err := svc.MessageAttachments(rctx, []string{messageID})
	ok := err == nil && len(found[messageID]) > 0 &&
		svc.MayReadAttachments(rctx, messageID, found[messageID][0].MessageProjectID)
	if err != nil {
		d.s.messageLog.Error("Failed to list message attachments for delivery", "message", messageID, "error", err)
	}
	memo[messageID] = ok
	return ok
}

// brokerReceivesAttachments reports whether broker brokerID advertises the
// attachment delivery capability. A broker that does not (an older one)
// would ignore the wire field. A lookup failure answers false.
func (s *Server) brokerReceivesAttachments(ctx context.Context, brokerID string) bool {
	if brokerID == "" {
		return false
	}
	b, err := s.store.GetRuntimeBroker(ctx, brokerID)
	if err != nil || b == nil || b.Capabilities == nil {
		return false
	}
	return b.Capabilities.AttachmentDelivery
}

// Plan decides each delivery once for recipient: deliverable when the
// recipient passes the read decision and its broker advertises the
// attachment delivery capability; undeliverable_runtime when it passes but
// the broker does not; withheld when the decision answers no. The render
// site lists the plan in the envelope and stores it on the message for the
// dispatcher.
func (d *attachmentDelivery) Plan(ctx context.Context, recipient *store.Agent, deliveries []messages.AttachmentDelivery) []messages.AttachmentPlanEntry {
	if recipient == nil || len(deliveries) == 0 {
		return nil
	}
	rctx := attachmentDecisionForRecipient(recipient)
	memo := map[string]bool{}
	receives := d.s.brokerReceivesAttachments(ctx, recipient.RuntimeBrokerID)
	plan := make([]messages.AttachmentPlanEntry, 0, len(deliveries))
	for _, del := range deliveries {
		name := reduceAttachmentName(del.Name)
		e := messages.AttachmentPlanEntry{Delivery: del, Name: name}
		switch {
		case !d.recipientMayRead(rctx, del.MessageID, memo):
			e.State = messages.AttachmentWithheld
		case !receives:
			e.State = messages.AttachmentUndeliverableRuntime
			e.InboxPath = attachmentInboxPath(del.MessageID, name)
		default:
			e.State = messages.AttachmentDeliverable
			e.InboxPath = attachmentInboxPath(del.MessageID, name)
		}
		applyProjection(&e)
		plan = append(plan, e)
	}
	return plan
}

// applyProjection copies the delivery's scratchpad projection
// (ptone/scion#5830) onto plan entry e, which the envelope lists after the
// inbox path; a withheld recipient is told nothing about the attachment.
func applyProjection(e *messages.AttachmentPlanEntry) {
	if e.State == messages.AttachmentWithheld {
		return
	}
	e.ProjectedPath, e.ProjectedMount = e.Delivery.ProjectedPath, e.Delivery.ProjectedMount
}

// Issue returns the wire attachments of plan for one dispatch attempt to
// recipient on broker brokerID: one fresh capability per deliverable entry,
// after asking the recipient decision again. An entry whose decision
// turned to no since the plan gets no URL, which the broker reports as not
// delivered. Undeliverable and withheld entries get nothing.
func (d *attachmentDelivery) Issue(ctx context.Context, recipient *store.Agent, brokerID string, plan []messages.AttachmentPlanEntry) []messages.WireAttachment {
	if recipient == nil || len(plan) == 0 {
		return nil
	}
	rctx := attachmentDecisionForRecipient(recipient)
	memo := map[string]bool{}
	var out []messages.WireAttachment
	for _, e := range plan {
		if e.State != messages.AttachmentDeliverable {
			continue
		}
		w := messages.WireAttachment{
			ID: e.Delivery.ArtifactID, Name: e.Name, Mime: e.Delivery.MimeType,
			Size: e.Delivery.Size, SHA256: e.Delivery.SHA256, Path: e.InboxPath,
		}
		if brokerID != "" && d.recipientMayRead(rctx, e.Delivery.MessageID, memo) {
			nonce := make([]byte, 16)
			if _, err := rand.Read(nonce); err == nil {
				token, err := d.s.signDeliveryCapability(deliveryCapability{
					ArtifactID: e.Delivery.ArtifactID, MessageID: e.Delivery.MessageID,
					AgentID: recipient.ID, BrokerID: brokerID,
					Exp:   time.Now().Add(attachmentDeliveryTTL).Unix(),
					Nonce: base64.RawURLEncoding.EncodeToString(nonce),
				})
				switch {
				case err == nil:
					w.URL = attachmentDeliveriesPrefix + token
				case errors.Is(err, errCapabilityIssuanceClosed):
					// The active generation reached its sign_until: the
					// instance is already not ready, so say it once
					// rather than on every dispatch attempt.
					if deliveryIssuanceClosedLogged.CompareAndSwap(false, true) {
						d.s.messageLog.Error("Failed to sign an attachment delivery capability", "error", err)
					}
				default:
					d.s.messageLog.Error("Failed to sign an attachment delivery capability", "error", err)
				}
			}
		}
		out = append(out, w)
	}
	return out
}

// applyAttachmentPlan plans the deliveries of msg, the per-recipient message
// dispatched to recipient, and stores the plan on it for the render site and
// the dispatcher. It never changes msg otherwise: when the hub renders no
// delivery text (the envelope switch is off), the dispatcher applies the
// plan to the copy it sends (legacyAttachmentCopy), so copies made from msg
// for other recipients never carry this recipient's paths. It is called
// once per dispatched message, before any render.
func (s *Server) applyAttachmentPlan(ctx context.Context, recipient *store.Agent, msg *messages.StructuredMessage) {
	if msg == nil || len(msg.AttachmentDeliveries) == 0 || msg.AttachmentPlan != nil {
		return
	}
	msg.AttachmentPlan = s.attachmentDelivery().Plan(ctx, recipient, msg.AttachmentDeliveries)
}

// legacyAttachmentCopy returns the message the dispatcher sends for msg: msg
// itself when it carries rendered delivery text or no plan; otherwise (the
// broker formats the legacy envelope) a copy with the plan applied: its
// deliverable inbox paths in Attachments and its not-delivered lines in the
// body. msg is never changed, so a retry applies the plan afresh.
func legacyAttachmentCopy(msg *messages.StructuredMessage) *messages.StructuredMessage {
	if msg == nil || msg.DeliveryText != "" || len(msg.AttachmentPlan) == 0 {
		return msg
	}
	cp := *msg
	cp.Attachments = append([]string(nil), msg.Attachments...)
	messaging.ApplyAttachmentPlanLegacy(&cp, cp.AttachmentPlan)
	return &cp
}

// writeDeliveryNotFound is the one answer of the redemption route to every
// refusal.
func writeDeliveryNotFound(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, ErrCodeNotFound, "attachment delivery not found", nil)
}

// handleAttachmentDelivery serves GET /api/v1/attachment-deliveries/<token>:
// a broker redeeming a delivery capability. Every check is required and
// every refusal is the same 404:
//  1. the request carries a valid broker credential whose broker id is the
//     capability's;
//  2. the MAC verifies and the capability has not expired;
//  3. its nonce has not been redeemed (recorded before any byte is served,
//     so two concurrent redemptions serve once);
//  4. the message still exists and is not deleted, and the attachment is
//     still bound to it.
func (s *Server) handleAttachmentDelivery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}
	ctx := r.Context()
	broker := GetBrokerIdentityFromContext(ctx)
	token := strings.TrimPrefix(r.URL.Path, attachmentDeliveriesPrefix)
	if broker == nil || token == "" || strings.Contains(token, "/") {
		writeDeliveryNotFound(w)
		return
	}
	c, ok := s.verifyDeliveryCapability(token, time.Now())
	if !ok || c.BrokerID != broker.BrokerID() {
		writeDeliveryNotFound(w)
		return
	}
	svc := s.attachmentService()
	first, err := svc.RedeemDeliveryNonce(ctx, c.Nonce)
	if err != nil {
		if !errors.Is(err, artifacts.ErrUnavailable) {
			s.messageLog.Error("Failed to record an attachment delivery redemption", "error", err)
		}
		writeDeliveryNotFound(w)
		return
	}
	if !first {
		writeDeliveryNotFound(w)
		return
	}
	if _, live := s.liveMessageForAttachments(ctx, c.MessageID); !live {
		writeDeliveryNotFound(w)
		return
	}
	if svc.ServeBoundAttachment(w, r, c.ArtifactID, c.MessageID) == artifacts.ServeNotFound {
		writeDeliveryNotFound(w)
	}
}

// sweepDeliveryRedemptions deletes redemption records older than
// deliveryRedemptionRetention; part of the artifact maintenance pass.
func (s *Server) sweepDeliveryRedemptions(ctx context.Context) {
	if s.ArtifactStore() == nil {
		return
	}
	if _, err := s.attachmentService().SweepDeliveryRedemptions(ctx, time.Now().Add(-deliveryRedemptionRetention)); err != nil {
		s.messageLog.Error("Failed to sweep attachment delivery redemptions", "error", err)
	}
}

// dispatchAttachmentsKey carries the wire attachments of one dispatch
// attempt from the dispatcher to the broker transport.
type dispatchAttachmentsKey struct{}

// withDispatchAttachments attaches the wire attachments of one dispatch
// attempt to ctx.
func withDispatchAttachments(ctx context.Context, wire []messages.WireAttachment) context.Context {
	if len(wire) == 0 {
		return ctx
	}
	return context.WithValue(ctx, dispatchAttachmentsKey{}, wire)
}

// dispatchAttachmentsFromContext returns the wire attachments attached with
// withDispatchAttachments, or nil.
func dispatchAttachmentsFromContext(ctx context.Context) []messages.WireAttachment {
	if ctx == nil {
		return nil
	}
	wire, _ := ctx.Value(dispatchAttachmentsKey{}).([]messages.WireAttachment)
	return wire
}

// noteDeferredAttachments lists, on each deferred message of items, its
// attachments stored as artifacts (reduced name and size), when the caller
// of ctx reads them under the shared read decision (decision D15,
// ptone/scion#4596). A deferred message is never dispatched, so its
// attachments are never delivered; catch-up says so. A non-reader gets no
// field at all.
func (s *Server) noteDeferredAttachments(ctx context.Context, items []store.Message) {
	if !s.attachmentArtifactsReady() {
		return
	}
	var ids []string
	for i := range items {
		if items[i].DispatchState == store.MessageDispatchDeferred {
			ids = append(ids, items[i].ID)
		}
	}
	if len(ids) == 0 {
		return
	}
	svc := s.attachmentService()
	found, err := svc.MessageAttachments(ctx, ids)
	if err != nil {
		s.messageLog.Error("Failed to list deferred message attachments", "error", err)
		return
	}
	ctx = withStandingMemo(ctx)
	for i := range items {
		list := found[items[i].ID]
		if len(list) == 0 || !svc.MayReadAttachments(ctx, items[i].ID, list[0].MessageProjectID) {
			continue
		}
		for _, a := range list {
			items[i].DeferredAttachments = append(items[i].DeferredAttachments,
				store.DeferredAttachment{Name: reduceAttachmentName(a.Name), Size: a.Size})
		}
	}
}

// InboxRetentionDays returns messaging.attachment_inbox_retention_days, sent
// to the broker with a message's attachments so it prunes the agent's
// inbox (ptone/scion#4596).
func (d *attachmentDelivery) InboxRetentionDays() int {
	if ops := d.s.GetOperationalSettings(); ops != nil {
		return ops.AttachmentInboxRetentionDays()
	}
	return DefaultAttachmentInboxRetentionDays
}

// dispatchRetentionKey carries the inbox retention of one dispatch attempt.
type dispatchRetentionKey struct{}

// withDispatchRetention attaches the inbox retention to ctx.
func withDispatchRetention(ctx context.Context, days int) context.Context {
	if days <= 0 {
		return ctx
	}
	return context.WithValue(ctx, dispatchRetentionKey{}, days)
}

// dispatchRetentionFromContext returns the inbox retention attached with
// withDispatchRetention, or 0.
func dispatchRetentionFromContext(ctx context.Context) int {
	if ctx == nil {
		return 0
	}
	days, _ := ctx.Value(dispatchRetentionKey{}).(int)
	return days
}
