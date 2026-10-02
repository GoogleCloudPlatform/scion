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

//go:build !no_sqlite

package hub

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/go-jose/go-jose/v4/jwt"
	"google.golang.org/api/googleapi"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// Fakes (in-memory only, live in _test.go, with call counters for the
// zero-call and zero-mint assertions).
// ---------------------------------------------------------------------------

// fakeGCSMintCall records one GenerateAccessToken call.
type fakeGCSMintCall struct {
	email  string
	scopes []string
}

// fakeGCSTokenGenerator is a GCPTokenGenerator that records every mint call
// instead of talking to IAM. Handler-level tests substitute this so a
// zero-mint assertion is a plain call-count check.
type fakeGCSTokenGenerator struct {
	mu       sync.Mutex
	calls    []fakeGCSMintCall
	failMint bool
}

func (f *fakeGCSTokenGenerator) GenerateAccessToken(_ context.Context, email string, scopes []string) (*GCPAccessToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeGCSMintCall{email: email, scopes: append([]string(nil), scopes...)})
	if f.failMint {
		return nil, errors.New("fake gcs token generator: forced mint failure")
	}
	return &GCPAccessToken{AccessToken: "fake-token-for-" + email, ExpiresIn: 3600, TokenType: "Bearer"}, nil
}

func (f *fakeGCSTokenGenerator) GenerateIDToken(context.Context, string, string) (*GCPIDToken, error) {
	return &GCPIDToken{Token: "fake-id-token"}, nil
}

func (f *fakeGCSTokenGenerator) VerifyImpersonation(context.Context, string) error { return nil }

func (f *fakeGCSTokenGenerator) ServiceAccountEmail() string {
	return "hub-sa@test-hub-project.iam.gserviceaccount.com"
}

func (f *fakeGCSTokenGenerator) mintCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeGCSTokenGenerator) lastCall() (fakeGCSMintCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return fakeGCSMintCall{}, false
	}
	return f.calls[len(f.calls)-1], true
}

// fakeGCSBlockingTokenGenerator simulates a hung IAM call: GenerateAccessToken
// blocks until its ctx is done, then returns ctx.Err(), instead of returning
// immediately like fakeGCSTokenGenerator. Unblock forces any in-flight or
// future call to return even if ctx is never canceled, so a test can
// guarantee (e.g. via t.Cleanup) that no call outlives it.
type fakeGCSBlockingTokenGenerator struct {
	mu              sync.Mutex
	calls           int
	inFlight        int
	maxInFlightSeen int
	// entered receives one value per GenerateAccessToken call, the instant
	// it starts (before it blocks on ctx/unblock), so a test can wait until
	// the mint has actually been reached before acting (e.g. canceling the
	// caller's context) instead of racing it with a sleep. Buffered and
	// sent non-blocking so a generator under concurrent use never stalls on
	// a slow or absent reader.
	entered     chan struct{}
	unblock     chan struct{}
	unblockOnce sync.Once
}

func newFakeGCSBlockingTokenGenerator() *fakeGCSBlockingTokenGenerator {
	return &fakeGCSBlockingTokenGenerator{
		entered: make(chan struct{}, 64),
		unblock: make(chan struct{}),
	}
}

// Unblock forces every blocked and future GenerateAccessToken call to return
// immediately. Safe to call more than once (e.g. from t.Cleanup regardless
// of whether the test itself already relied on ctx being canceled).
func (f *fakeGCSBlockingTokenGenerator) Unblock() {
	f.unblockOnce.Do(func() { close(f.unblock) })
}

func (f *fakeGCSBlockingTokenGenerator) GenerateAccessToken(ctx context.Context, _ string, _ []string) (*GCPAccessToken, error) {
	f.mu.Lock()
	f.calls++
	f.inFlight++
	if f.inFlight > f.maxInFlightSeen {
		f.maxInFlightSeen = f.inFlight
	}
	f.mu.Unlock()
	select {
	case f.entered <- struct{}{}:
	default:
	}
	defer func() {
		f.mu.Lock()
		f.inFlight--
		f.mu.Unlock()
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-f.unblock:
		return nil, errors.New("fakeGCSBlockingTokenGenerator: unblocked by test cleanup")
	}
}

func (f *fakeGCSBlockingTokenGenerator) GenerateIDToken(context.Context, string, string) (*GCPIDToken, error) {
	return nil, errors.New("fakeGCSBlockingTokenGenerator: GenerateIDToken not implemented")
}

func (f *fakeGCSBlockingTokenGenerator) VerifyImpersonation(context.Context, string) error {
	return nil
}

func (f *fakeGCSBlockingTokenGenerator) ServiceAccountEmail() string {
	return "hub-sa@test-hub-project.iam.gserviceaccount.com"
}

func (f *fakeGCSBlockingTokenGenerator) mintCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// maxInFlight returns the high-water mark of concurrently in-progress
// GenerateAccessToken calls observed so far: the number of calls that had
// entered but not yet returned, at the single busiest moment. If calls were
// serialized behind one another (e.g. by a mutex around the mint), this
// never exceeds 1 regardless of how many callers were waiting.
func (f *fakeGCSBlockingTokenGenerator) maxInFlight() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxInFlightSeen
}

// fakeGCSStoredObject is one object a fakeGCSSource knows about.
type fakeGCSStoredObject struct {
	attrs *storage.ObjectAttrs
	body  []byte
}

// fakeGCSSource is the in-memory gcsObjectSource fake. attrsErr/openErr,
// when set, apply to every call regardless of bucket/object, simulating a
// GCS-side denial or outage. attrsBlock, when set, makes every Attrs call
// wait until it is closed — used to hold the global concurrency semaphore
// open for the rate-limit/concurrency test below.
type fakeGCSSource struct {
	mu         sync.Mutex
	objects    map[string]fakeGCSStoredObject
	attrsCalls int
	openCalls  int
	attrsErr   error
	openErr    error
	attrsBlock chan struct{}
	// openRequiredGeneration, when set, is the generation Open compares the
	// caller's requested generation against, instead of the stored object's
	// own attrs.Generation. This is what lets a test simulate a concurrent
	// overwrite: Attrs() keeps reporting the generation observed before the
	// "overwrite" (from the stored object), while Open() independently
	// requires the new one, exactly like a real GCS object whose live
	// generation changed between the two calls.
	openRequiredGeneration *int64
	// openStreamFailErr, when set, makes the reader Open returns fail with
	// this error once the stored body is exhausted, instead of a clean EOF —
	// simulating a network stream that fails partway through, after a
	// successful Attrs/Open and after some real bytes have already gone out.
	openStreamFailErr error
}

// failingAfterReader returns real data from r, then fails with err instead
// of a clean io.EOF once r is exhausted — see fakeGCSSource.openStreamFailErr.
type failingAfterReader struct {
	r   io.Reader
	err error
}

func (f *failingAfterReader) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if err == io.EOF {
		return n, f.err
	}
	return n, err
}

func newFakeGCSSource() *fakeGCSSource {
	return &fakeGCSSource{objects: make(map[string]fakeGCSStoredObject)}
}

func gcsFakeObjectKey(bucket, object string) string { return bucket + "/" + object }

func (f *fakeGCSSource) putObject(bucket, object string, attrs *storage.ObjectAttrs, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[gcsFakeObjectKey(bucket, object)] = fakeGCSStoredObject{attrs: attrs, body: body}
}

func (f *fakeGCSSource) Attrs(_ context.Context, bucket, object string) (*storage.ObjectAttrs, error) {
	f.mu.Lock()
	f.attrsCalls++
	err := f.attrsErr
	block := f.attrsBlock
	obj, ok := f.objects[gcsFakeObjectKey(bucket, object)]
	f.mu.Unlock()

	if block != nil {
		<-block
	}
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, storage.ErrObjectNotExist
	}
	return obj.attrs, nil
}

func (f *fakeGCSSource) Open(_ context.Context, bucket, object string, generation int64) (io.ReadCloser, error) {
	f.mu.Lock()
	f.openCalls++
	err := f.openErr
	obj, ok := f.objects[gcsFakeObjectKey(bucket, object)]
	required := int64(0)
	if ok {
		required = obj.attrs.Generation
	}
	if f.openRequiredGeneration != nil {
		required = *f.openRequiredGeneration
	}
	f.mu.Unlock()

	if err != nil {
		return nil, err
	}
	if !ok || required != generation {
		return nil, &googleapi.Error{Code: http.StatusPreconditionFailed, Message: "generation mismatch"}
	}
	if f.openStreamFailErr != nil {
		return io.NopCloser(&failingAfterReader{r: bytes.NewReader(obj.body), err: f.openStreamFailErr}), nil
	}
	return io.NopCloser(bytes.NewReader(obj.body)), nil
}

func (f *fakeGCSSource) counts() (attrsCalls, openCalls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attrsCalls, f.openCalls
}

// newFakeGCSSourceFactory bridges a fake gcsSourceFactory to gen, so a mint
// failure and a mint-call assertion both work at the handler level without a
// real IAM call or a real *storage.Client. The factory is only ever reached
// after every authorization step has passed (see handleGCSObject step 10),
// so its call count doubles as the "zero mint" assertion for every deny path
// above it.
func newFakeGCSSourceFactory(gen GCPTokenGenerator, source *fakeGCSSource) gcsSourceFactory {
	return func(ctx context.Context, saEmail string) (gcsObjectSource, func(), error) {
		if _, err := gen.GenerateAccessToken(ctx, saEmail, []string{gcsLinkReadOnlyScope}); err != nil {
			return nil, func() {}, err
		}
		return source, func() {}, nil
	}
}

// gcsAuditRecorder captures every GCSLinkFetchEvent for assertions. It embeds
// a real LogAuditLogger so it satisfies the full AuditLogger interface with
// production behaviour for every other event type — the same technique
// recordingMaterialAuditor (material_test_helpers_test.go) uses.
type gcsAuditRecorder struct {
	*LogAuditLogger
	mu     sync.Mutex
	events []*GCSLinkFetchEvent
}

func newGCSAuditRecorder() *gcsAuditRecorder {
	return &gcsAuditRecorder{LogAuditLogger: NewLogAuditLogger("[test]", false)}
}

func (r *gcsAuditRecorder) LogGCSLinkFetchEvent(ctx context.Context, e *GCSLinkFetchEvent) error {
	r.mu.Lock()
	r.events = append(r.events, e)
	r.mu.Unlock()
	return r.LogAuditLogger.LogGCSLinkFetchEvent(ctx, e)
}

func (r *gcsAuditRecorder) all() []*GCSLinkFetchEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*GCSLinkFetchEvent, len(r.events))
	copy(out, r.events)
	return out
}

// ---------------------------------------------------------------------------
// Fixture wiring
// ---------------------------------------------------------------------------

// setGCSLinksExperiment overrides the web.gcs_links experiment to enabled on
// srv, via an admin-override document on a fresh OperationalSettings backed
// by a fake store — the same mechanism an admin toggling it from Admin ->
// Server Config -> Experiments would produce. srv.experiments is left nil,
// so every other registered experiment (and every field of this one besides
// its resolved value) still comes from the real production registry;
// nothing here can mask a registry-definition bug the way a parallel test
// registry would.
func setGCSLinksExperiment(t *testing.T, srv *Server, enabled bool) {
	t.Helper()
	fakeStore := newFakeHubSettingStore()
	fakeStore.seed("experiments", json.RawMessage(fmt.Sprintf(`{"overrides":{%q:%t}}`, gcsLinksExperiment, enabled)))
	ops := NewOperationalSettings(fakeStore, emptyKoanf(), emptyKoanf())
	if _, err := ops.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	srv.SetOperationalSettings(ops)
}

// gcsFixture bundles a chat-capable test server with the gs:// link fakes
// wired in place of the real GCP token generator and storage client.
type gcsFixture struct {
	srv    *Server
	store  store.Store
	gen    *fakeGCSTokenGenerator
	source *fakeGCSSource
	audit  *gcsAuditRecorder
}

// newGCSFixture builds a fixture with the feature enabled: a fake token
// generator is set (s.gcpTokenGenerator != nil) and the web.gcs_links
// experiment is overridden on, via setGCSLinksExperiment, since the
// production registry default is off (ptone/scion#2545).
func newGCSFixture(t *testing.T) *gcsFixture {
	t.Helper()
	srv, s := attachmentTestServer(t)
	gen := &fakeGCSTokenGenerator{}
	source := newFakeGCSSource()
	audit := newGCSAuditRecorder()
	srv.SetGCPTokenGenerator(gen)
	srv.gcsLinkSourceFactory = newFakeGCSSourceFactory(gen, source)
	srv.SetAuditLogger(audit)
	setGCSLinksExperiment(t, srv, true)
	return &gcsFixture{srv: srv, store: s, gen: gen, source: source, audit: audit}
}

// gcsTestProject creates a project and its members group (the latter is
// normally set up by the project-creation handler, not by the store call
// alone — addProjectMemberWithRole needs it to exist).
func gcsTestProject(t *testing.T, f *gcsFixture, seed string) *store.Project {
	t.Helper()
	p := &store.Project{ID: tid(seed), Name: seed, Slug: seed}
	require.NoError(t, f.store.CreateProject(context.Background(), p))
	f.srv.createProjectMembersGroup(context.Background(), p)
	return p
}

func gcsTestUser(t *testing.T, s store.Store, seed string) *store.User {
	t.Helper()
	u := &store.User{ID: tid(seed), Email: seed + "@test.com", DisplayName: seed, Role: store.UserRoleMember, Status: "active"}
	require.NoError(t, s.CreateUser(context.Background(), u))
	return u
}

func gcsTestAgent(t *testing.T, s store.Store, project *store.Project, seed string, gcpIdentity *store.GCPIdentityConfig) *store.Agent {
	t.Helper()
	a := &store.Agent{ID: tid(seed), Slug: seed, Name: seed, ProjectID: project.ID, Phase: "running"}
	if gcpIdentity != nil {
		a.AppliedConfig = &store.AgentAppliedConfig{GCPIdentity: gcpIdentity}
	}
	require.NoError(t, s.CreateAgent(context.Background(), a))
	return a
}

func gcsTestSA(t *testing.T, s store.Store, project *store.Project, seed, email string) *store.GCPServiceAccount {
	t.Helper()
	sa := &store.GCPServiceAccount{
		ID: tid(seed), Scope: store.ScopeProject, ScopeID: project.ID, Email: email,
		ProjectID: project.ID, CreatedAt: time.Now(),
	}
	require.NoError(t, s.CreateGCPServiceAccount(context.Background(), sa))
	return sa
}

func gcsTestTopic(t *testing.T, s store.Store, project *store.Project, seed string) *store.Conversation {
	t.Helper()
	now := time.Now().UTC()
	conv := &store.Conversation{
		ID: tid(seed), ProjectID: &project.ID, Kind: "group", Surface: "native",
		DisplayName: seed, DriftState: "active", LastActivityAt: now, CreatedAt: now,
	}
	require.NoError(t, s.CreateConversation(context.Background(), conv))
	return conv
}

func gcsTestDM(t *testing.T, s store.Store, seed, externalRef string) *store.Conversation {
	t.Helper()
	now := time.Now().UTC()
	conv := &store.Conversation{
		ID: tid(seed), Kind: "direct", Surface: "native", ExternalRef: externalRef,
		DriftState: "active", LastActivityAt: now, CreatedAt: now,
	}
	require.NoError(t, s.CreateConversation(context.Background(), conv))
	return conv
}

// gcsMessageSpec is the flexible message shape used to construct every
// negative-test fixture (forged sender, user-sent, empty conversation id) as
// well as the ordinary agent-sent case.
type gcsMessageSpec struct {
	seed           string
	projectID      string
	sender         string
	senderID       string
	body           string
	agentID        string
	conversationID string
}

func gcsTestMessage(t *testing.T, s store.Store, spec gcsMessageSpec) *store.Message {
	t.Helper()
	msg := &store.Message{
		ID: tid(spec.seed), ProjectID: spec.projectID, Sender: spec.sender, SenderID: spec.senderID,
		Recipient: "user:nobody", Msg: spec.body, Type: "instruction", AgentID: spec.agentID,
		ConversationID: spec.conversationID, CreatedAt: time.Now(),
	}
	require.NoError(t, s.CreateMessage(context.Background(), msg))
	return msg
}

func gcsRequestPath(messageID, bucket, object string) string {
	v := url.Values{"message": {messageID}, "bucket": {bucket}, "object": {object}}
	return "/api/v1/gcs/object?" + v.Encode()
}

// gcsAssignedIdentity is a convenience constructor for an "assign" mode
// GCPIdentityConfig.
func gcsAssignedIdentity(sa *store.GCPServiceAccount) *store.GCPIdentityConfig {
	return &store.GCPIdentityConfig{
		MetadataMode:        store.GCPMetadataModeAssign,
		ServiceAccountID:    sa.ID,
		ServiceAccountEmail: sa.Email,
	}
}

// gcsHappyPathFixture builds the base allowed scenario used by most non-deny
// tests: agent "sender-agent" (assign mode, SA sa-a) posts uri in project
// topic "topic-1"; viewer is a project member. Returns the viewer and the
// message so the caller only has to build the query string.
func gcsHappyPathFixture(t *testing.T, f *gcsFixture, seed, uri string) (viewer *store.User, msg *store.Message) {
	t.Helper()
	project := gcsTestProject(t, f, seed+"-project")
	sa := gcsTestSA(t, f.store, project, seed+"-sa-a", "sa-a-"+seed+"@test.iam.gserviceaccount.com")
	agent := gcsTestAgent(t, f.store, project, seed+"-agent", gcsAssignedIdentity(sa))
	conv := gcsTestTopic(t, f.store, project, seed+"-topic")
	msg = gcsTestMessage(t, f.store, gcsMessageSpec{
		seed: seed + "-msg", projectID: project.ID, sender: "agent:" + agent.Slug, senderID: agent.ID,
		body: "see " + uri, agentID: agent.ID, conversationID: conv.ID,
	})
	viewer = gcsTestUser(t, f.store, seed+"-viewer")
	addProjectMemberWithRole(t, f.store, project, viewer.ID, store.GroupMemberRoleMember)
	return viewer, msg
}

// ---------------------------------------------------------------------------
// Parity vector table: one table of {body, bucket, object, linked,
// serverAllowed} cases, present verbatim in this file and in
// web/src/utils/chat-file-links.test.ts. The `name` field is the
// cross-reference key: the same name labels the
// matching case in the vitest file's GCS_PARITY_VECTORS table. `linked` here
// documents the value the web-side row asserts; only `serverAllowed` is
// exercised by this Go test.
// ---------------------------------------------------------------------------

type gcsParityVector struct {
	name          string
	body          string
	bucket        string
	object        string
	linked        bool // documented for cross-reference; asserted in chat-file-links.test.ts
	serverAllowed bool // asserted here via bodyReferencesGCSURI
}

var gcsParityVectors = []gcsParityVector{
	// Required parity vector: a bucket name containing a hyphenated
	// cross-project-style segment, to prove the bucket pattern accepts it.
	// Cross-reference: chat-file-links.test.ts, row "cross-project-exchange-uri".
	{
		name:          "cross-project-exchange-uri",
		body:          "see gs://scion-xproject-exchange/workspace-volumes/dev-brief.md",
		bucket:        "scion-xproject-exchange",
		object:        "workspace-volumes/dev-brief.md",
		linked:        true,
		serverAllowed: true,
	},
	{name: "exact-object", body: "gs://bkt/abc.md", bucket: "bkt", object: "abc.md", linked: true, serverAllowed: true},
	{name: "prefix-single-char", body: "gs://bkt/abc.md", bucket: "bkt", object: "a", linked: false, serverAllowed: false},
	{name: "prefix-stem", body: "gs://bkt/abc.md", bucket: "bkt", object: "abc", linked: false, serverAllowed: false},
	{name: "prefix-partial-extension", body: "gs://bkt/abc.md", bucket: "bkt", object: "abc.m", linked: false, serverAllowed: false},
	{name: "scheme-prefixed-not-a-link", body: "xgs://bkt/o", bucket: "bkt", object: "o", linked: false, serverAllowed: false},
	{name: "leading-slash-not-a-link", body: "/gs://bkt/o", bucket: "bkt", object: "o", linked: false, serverAllowed: false},
	{name: "parenthesized-trailing-dot", body: "(gs://bkt/o.md).", bucket: "bkt", object: "o.md", linked: true, serverAllowed: true},
	{name: "nested-workspace-path-no-collision", body: "gs://bkt/workspace/x.md, ok", bucket: "bkt", object: "workspace/x.md", linked: true, serverAllowed: true},
	{name: "trailing-slash-directory", body: "gs://bkt/dir/", bucket: "bkt", object: "dir", linked: false, serverAllowed: false},
	{name: "backtick-wrapped", body: "`gs://scion-xproject-exchange/workspace-volumes/dev-brief.md`", bucket: "scion-xproject-exchange", object: "workspace-volumes/dev-brief.md", linked: true, serverAllowed: true},
	// A trailing underscore is a word character, so it blocks the right
	// boundary exactly like a trailing letter would: the real link in this
	// body is "abc.md_extra", not "abc.md".
	{name: "underscore-blocks-right-boundary", body: "gs://bkt/abc.md_extra", bucket: "bkt", object: "abc.md", linked: false, serverAllowed: false},
	// A leading underscore likewise blocks the left boundary.
	{name: "underscore-blocks-left-boundary", body: "x_gs://bkt/o.txt", bucket: "bkt", object: "o.txt", linked: false, serverAllowed: false},
	// A trailing run of `~+=@%` is part of the object, not a boundary — the
	// full object links and is allowed; a truncated form of the same posted
	// text is denied.
	{name: "trailing-tilde-full-object", body: "gs://bkt/secret~", bucket: "bkt", object: "secret~", linked: true, serverAllowed: true},
	{name: "trailing-tilde-truncated-object", body: "gs://bkt/secret~", bucket: "bkt", object: "secret", linked: false, serverAllowed: false},
	{name: "trailing-plus-full-object", body: "gs://bkt/data+", bucket: "bkt", object: "data+", linked: true, serverAllowed: true},
	{name: "trailing-plus-truncated-object", body: "gs://bkt/data+", bucket: "bkt", object: "data", linked: false, serverAllowed: false},
	{name: "trailing-double-equals-full-object", body: "gs://bkt/key==", bucket: "bkt", object: "key==", linked: true, serverAllowed: true},
	{name: "trailing-double-equals-truncated-object", body: "gs://bkt/key==", bucket: "bkt", object: "key", linked: false, serverAllowed: false},
	{name: "trailing-percent-full-object", body: "gs://bkt/report%", bucket: "bkt", object: "report%", linked: true, serverAllowed: true},
	{name: "trailing-percent-truncated-object", body: "gs://bkt/report%", bucket: "bkt", object: "report", linked: false, serverAllowed: false},
	{name: "trailing-at-full-object", body: "gs://bkt/a@", bucket: "bkt", object: "a@", linked: true, serverAllowed: true},
	{name: "trailing-at-truncated-object", body: "gs://bkt/a@", bucket: "bkt", object: "a", linked: false, serverAllowed: false},
	// A fragment-, query- or param-like continuation immediately after the
	// object — '#', '?' or '&' followed by a further non-whitespace
	// character — is a full reject, not a truncation.
	{name: "fragment-continuation-hash", body: "gs://bkt/a#frag", bucket: "bkt", object: "a", linked: false, serverAllowed: false},
	{name: "fragment-continuation-query", body: "gs://bkt/a?x=1", bucket: "bkt", object: "a", linked: false, serverAllowed: false},
	{name: "fragment-continuation-amp", body: "gs://bkt/a&b", bucket: "bkt", object: "a", linked: false, serverAllowed: false},
	// A trailing '?' at the very end of a sentence (nothing after it, or only
	// whitespace) is not a continuation — it links normally, truncated at '?'
	// exactly like a period or comma would be.
	{name: "trailing-question-end-of-sentence", body: "see gs://bkt/o.md?", bucket: "bkt", object: "o.md", linked: true, serverAllowed: true},
	{name: "double-quoted-wrapping", body: `"gs://bkt/o.md"`, bucket: "bkt", object: "o.md", linked: true, serverAllowed: true},
	{name: "parenthesized-wrapping", body: "(gs://bkt/o.md)", bucket: "bkt", object: "o.md", linked: true, serverAllowed: true},
	// A name containing whitespace links, and is served, only up to the
	// whitespace — documented as an accepted edge, not a truncation bug: the
	// agent posted this text, and only a viewer of this message can fetch
	// it, so this does not widen access beyond what renders.
	{name: "space-in-name-accepted-edge", body: "gs://bkt/my file.txt", bucket: "bkt", object: "my", linked: true, serverAllowed: true},
	// A gs:// URI nested inside another URI's object run belongs entirely to
	// the outer candidate's raw run — the scan never re-starts partway
	// through an already-consumed run, so the inner occurrence, including
	// one naming a different bucket, is never a candidate on its own.
	{name: "nested-tilde-inner-never-a-candidate", body: "gs://b1b/a~gs://b1b/c", bucket: "b1b", object: "c", linked: false, serverAllowed: false},
	{name: "nested-tilde-outer-allowed", body: "gs://b1b/a~gs://b1b/c", bucket: "b1b", object: "a~gs://b1b/c", linked: true, serverAllowed: true},
	{name: "nested-comma-inner-never-a-candidate", body: "gs://b1b/a,gs://b1b/c", bucket: "b1b", object: "c", linked: false, serverAllowed: false},
	{name: "nested-comma-outer-allowed", body: "gs://b1b/a,gs://b1b/c", bucket: "b1b", object: "a,gs://b1b/c", linked: true, serverAllowed: true},
	{name: "nested-cross-bucket-inner-never-a-candidate", body: "gs://pub/x=gs://sec/key", bucket: "sec", object: "key", linked: false, serverAllowed: false},
	{name: "nested-cross-bucket-outer-allowed", body: "gs://pub/x=gs://sec/key", bucket: "pub", object: "x=gs://sec/key", linked: true, serverAllowed: true},
	// Two independent, space-separated occurrences: each is its own
	// candidate, both allowed — this is not the nested case above.
	{name: "multiple-independent-occurrences-first", body: "gs://bkt/a gs://bkt/b", bucket: "bkt", object: "a", linked: true, serverAllowed: true},
	{name: "multiple-independent-occurrences-second", body: "gs://bkt/a gs://bkt/b", bucket: "bkt", object: "b", linked: true, serverAllowed: true},
	// A markdown escape (raw '\') immediately after the object voids the
	// whole candidate on the server, which reads the raw body: an agent
	// posting "secret\_v2" (an escaped underscore) never authorizes a fetch
	// of "secret", even though the client, applying its regex directly to
	// this same raw text (no markdown rendering happens here), simply stops
	// its object-class run at the backslash — the same as it would at
	// whitespace — and links "secret". That is a third value, distinct from
	// what the client actually links once this text has gone through real
	// markdown rendering: "secret_v2", pinned separately in
	// gcs-link.pw.ts, where the escape is already resolved before the
	// linkifier ever runs.
	{name: "backslash-voids-candidate", body: `gs://bkt/secret\_v2`, bucket: "bkt", object: "secret", linked: true, serverAllowed: false},
	// The server's void-on-backslash rule covers the unescaped object too,
	// not only the truncated one: neither "secret" nor "secret_v2" is ever
	// authorized from this raw body. The client can never produce
	// "secret_v2" by applying the regex directly to raw, unrendered text
	// (only real markdown rendering resolves the escape), so this is false
	// on both sides, for different reasons.
	{name: "backslash-voids-candidate-unescaped-object", body: `gs://bkt/secret\_v2`, bucket: "bkt", object: "secret_v2", linked: false, serverAllowed: false},
	// A bucket not immediately followed by '/' is not a valid occurrence at
	// all: the whole match fails, not just a shorter bucket. Regression
	// guard for the scan that replays the client's global match on the
	// server (dropping this guard would let the server treat "bkt" as a
	// bucket and extract an object starting right after the ':' or '~').
	{name: "bucket-not-followed-by-slash-colon", body: "gs://bkt:o.txt", bucket: "bkt", object: "o.txt", linked: false, serverAllowed: false},
	{name: "bucket-not-followed-by-slash-tilde", body: "gs://bkt~o.txt", bucket: "bkt", object: "o.txt", linked: false, serverAllowed: false},
	// A question mark followed by more text, itself followed by whitespace,
	// is not a continuation: a further character exists after the '?', but
	// it is whitespace, so the continuation-reject clause never fires.
	{name: "trailing-question-then-whitespace", body: "is it gs://bkt/o.md? next", bucket: "bkt", object: "o.md", linked: true, serverAllowed: true},
	// Trailing sentence punctuation trimmed from the object is what the
	// continuation check must see next, not the character after that
	// punctuation — "a.#frag" trims the '.', then sees '.' (not '#') as the
	// immediate next character, so this is not a fragment continuation.
	{name: "trimmed-punct-then-fragment-hash", body: "gs://bkt/a.#frag", bucket: "bkt", object: "a", linked: true, serverAllowed: true},
	{name: "trimmed-punct-then-fragment-query", body: "gs://bkt/a,?x", bucket: "bkt", object: "a", linked: true, serverAllowed: true},
}

// TestGCSLink_StrikethroughServerAllowsLiteralTildes covers an accepted
// server-only allow: '~' is in the object class (both mid- and
// final-class), so the raw body's literal "~~" around a posted gs:// URI is
// simply part of the posted object text — a literal token the agent
// posted, not prefix borrowing. Not in the shared parity table: unlike
// every other row, this one depends on real markdown rendering (marked
// strips the "~~" delimiters before the client's linkifier ever runs),
// which a flat-string vitest test cannot represent — see the Chromium spec
// for the client's side of this same case.
func TestGCSLink_StrikethroughServerAllowsLiteralTildes(t *testing.T) {
	require.True(t, bodyReferencesGCSURI("~~gs://b1b/o~~", "b1b", "o~~"))
	require.False(t, bodyReferencesGCSURI("~~gs://b1b/o~~", "b1b", "o"))
}

// TestGCSLink_ClientWiderMismatches_ServerRejectsEntirely covers the server
// side of rendered-vs-raw mismatches where the client links an object the
// server denies, using the exact raw message bodies the e2e fixtures
// (data.ts) send to Chromium for the client side. Each case asserts that
// the server denies the object the client links:
//   - emphasis: the leading "_" is a word byte, so the left-boundary check
//     fails and the occurrence is never extracted.
//   - every other case: the object "a" is immediately followed by a "?" or
//     "&" marker, and the byte after that marker is not whitespace (a
//     backtick, "*", "a", "l" or "q"), so the continuation rule rejects it.
func TestGCSLink_ClientWiderMismatches_ServerRejectsEntirely(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		object string
	}{
		{name: "emphasis", body: "_gs://bkt/o_ done", object: "o"},
		{name: "tag-split-question", body: "gs://bkt/a?`x` done", object: "a"},
		{name: "raw-entity-amp", body: "gs://bkt/a&amp; done", object: "a"},
		{name: "strong-emphasis-after-question", body: "gs://bkt/a?**x** done", object: "a"},
		{name: "raw-entity-lt", body: "gs://bkt/a&lt;x done", object: "a"},
		{name: "raw-entity-quot", body: `gs://bkt/a&quot;x done`, object: "a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.False(t, bodyReferencesGCSURI(tc.body, "bkt", tc.object))
		})
	}
}

func TestGCSLink_ParityVectors(t *testing.T) {
	for _, v := range gcsParityVectors {
		t.Run(v.name, func(t *testing.T) {
			got := bodyReferencesGCSURI(v.body, v.bucket, v.object)
			if got != v.serverAllowed {
				t.Errorf("bodyReferencesGCSURI(%q, %q, %q) = %v, want %v", v.body, v.bucket, v.object, got, v.serverAllowed)
			}
		})
	}
}

// TestGCSLink_EveryWhitespaceByteTerminatesContinuation proves each byte in
// isGCSWhitespaceByte's switch individually, not just the ASCII space every
// other row happens to use: a continuation marker followed by any one of
// these still links normally, exactly as it does for a plain space.
// Removing any single case from that switch would otherwise go undetected.
func TestGCSLink_EveryWhitespaceByteTerminatesContinuation(t *testing.T) {
	for _, b := range []byte{' ', '\t', '\n', '\r', '\f', '\v'} {
		body := "gs://bkt/o.md?" + string(b) + "next"
		t.Run(fmt.Sprintf("byte_%#x", b), func(t *testing.T) {
			require.True(t, bodyReferencesGCSURI(body, "bkt", "o.md"))
		})
	}
}

// TestGCSLink_EveryTrimByteIsTrimmed proves each byte in
// isGCSObjectTrimByte's switch individually: the shared parity table only
// ever exercises '.' and ',' as the trailing trimmed character. Removing
// any of the other cases — ':', '!', '$', '*', '(', ')', '-' — from that
// switch would otherwise go undetected.
func TestGCSLink_EveryTrimByteIsTrimmed(t *testing.T) {
	for _, b := range []byte{'.', ',', ':', '!', '$', '*', '(', ')', '-'} {
		body := "gs://bkt/a" + string(b)
		t.Run(fmt.Sprintf("byte_%c", b), func(t *testing.T) {
			require.True(t, bodyReferencesGCSURI(body, "bkt", "a"))
		})
	}
}

// TestGCSLink_UppercaseObjectBytesAreObjectClass proves the client pattern's
// uppercase word-byte range (isGCSWordByte's 'A'-'Z' half, used by both
// isGCSObjectClassByte and isGCSObjectFinalByte): every existing parity row
// posts an all-lowercase object, so removing this range from the switch
// would otherwise go undetected.
func TestGCSLink_UppercaseObjectBytesAreObjectClass(t *testing.T) {
	require.True(t, bodyReferencesGCSURI("gs://bkt/ABC", "bkt", "ABC"))
}

// TestGCSLink_ParityVector_RequiredExchangeURIEndToEnd is the required
// end-to-end proof for the cross-project-exchange bucket URI: an agent posts
// it in a project topic, a project member requests exactly that
// bucket/object with the message id, and the fetch is allowed.
func TestGCSLink_ParityVector_RequiredExchangeURIEndToEnd(t *testing.T) {
	f := newGCSFixture(t)
	const bucket = "scion-xproject-exchange"
	const object = "workspace-volumes/dev-brief.md"
	viewer, msg := gcsHappyPathFixture(t, f, "xproject", "gs://"+bucket+"/"+object)
	f.source.putObject(bucket, object, &storage.ObjectAttrs{Size: 5, Generation: 1, ContentType: "text/markdown"}, []byte("# hi\n"))

	rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, bucket, object), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "# hi\n", rec.Body.String())
}

// ---------------------------------------------------------------------------
// Feature off.
// ---------------------------------------------------------------------------

func TestGCSLink_FeatureOff(t *testing.T) {
	srv, s := attachmentTestServer(t)
	// No SetGCPTokenGenerator call: s.gcpTokenGenerator stays nil. The
	// web.gcs_links experiment defaults off too (production registry
	// default), so this denies on either/both grounds.
	viewer := gcsTestUser(t, s, "off-viewer")

	rec := doRequestAsUser(t, srv, viewer, http.MethodGet, gcsRequestPath(tid("off-msg"), "bkt", "o.txt"), nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.JSONEq(t, `{"error":{"code":"not_found","message":"Object not found"}}`, rec.Body.String())
}

// TestGCSLink_Deny_FeatureOffDespiteValidFixture isolates the step-1
// feature-availability check specifically: every other step would succeed
// (real message, agent, SA and fake source object), so only
// s.gcpTokenGenerator == nil denies the fetch. The fake gcsLinkSourceFactory
// stays wired directly to f.gen, independent of s.gcpTokenGenerator, so a
// mint call here can only come from the handler failing to stop at step 1.
func TestGCSLink_Deny_FeatureOffDespiteValidFixture(t *testing.T) {
	f := newGCSFixture(t)
	viewer, msg := gcsHappyPathFixture(t, f, "featureoffvalid", "gs://bkt/o.txt")
	f.source.putObject("bkt", "o.txt", &storage.ObjectAttrs{Size: 2, Generation: 1}, []byte("hi"))

	f.srv.SetGCPTokenGenerator(nil)

	rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "o.txt"), nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.JSONEq(t, gcsExpected404Body, rec.Body.String())
	require.Zero(t, f.gen.mintCount())
	attrsCalls, openCalls := f.source.counts()
	require.Zero(t, attrsCalls)
	require.Zero(t, openCalls)

	events := f.audit.all()
	require.Len(t, events, 1)
	require.Equal(t, GCSLinkReasonFeatureOff, events[0].Reason)
}

// TestGCSLink_Deny_ExperimentOffDespiteValidFixture is
// TestGCSLink_Deny_FeatureOffDespiteValidFixture's counterpart for the other
// half of step 1's condition: every other step would succeed, and the token
// generator is present, so only the web.gcs_links experiment being off
// denies the fetch. newGCSFixture turns the experiment on by default (the
// production registry default is off); this flips it back off via the same
// admin-override mechanism to isolate the check (ptone/scion#2545).
func TestGCSLink_Deny_ExperimentOffDespiteValidFixture(t *testing.T) {
	f := newGCSFixture(t)
	viewer, msg := gcsHappyPathFixture(t, f, "experimentoffvalid", "gs://bkt/o.txt")
	f.source.putObject("bkt", "o.txt", &storage.ObjectAttrs{Size: 2, Generation: 1}, []byte("hi"))

	setGCSLinksExperiment(t, f.srv, false)

	rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "o.txt"), nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.JSONEq(t, gcsExpected404Body, rec.Body.String())
	require.Zero(t, f.gen.mintCount())
	attrsCalls, openCalls := f.source.counts()
	require.Zero(t, attrsCalls)
	require.Zero(t, openCalls)

	events := f.audit.all()
	require.Len(t, events, 1)
	require.Equal(t, GCSLinkReasonFeatureOff, events[0].Reason)
}

// TestGCSLink_MintFailure_502 covers the mint-failure case: every
// authorization step passes, but the token generator itself fails.
func TestGCSLink_MintFailure_502(t *testing.T) {
	f := newGCSFixture(t)
	viewer, msg := gcsHappyPathFixture(t, f, "mintfail", "gs://bkt/o.txt")
	f.source.putObject("bkt", "o.txt", &storage.ObjectAttrs{Size: 2, Generation: 1}, []byte("hi"))
	f.gen.failMint = true

	rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "o.txt"), nil)
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	attrsCalls, openCalls := f.source.counts()
	require.Zero(t, attrsCalls)
	require.Zero(t, openCalls)

	events := f.audit.all()
	require.Len(t, events, 1)
	require.Equal(t, GCSLinkReasonMintFailed, events[0].Reason)
}

// TestGCSLink_MintBoundedByRequestDeadlineAndReleasesSlot proves the token
// mint (step 10) is bounded by the same request ctx as everything else (no
// separate per-mint timeout), using the real production factory
// (gcsObjectSourceFor) with a token generator that never returns on its
// own. The request deadline is left at its production default: the test
// waits for the generator's "entered" signal — proof the mint has actually
// been reached — before canceling the request's own context, so it never
// races the store lookups in steps 5-9 the way a short whole-request
// deadline override would under load. It asserts the request still
// completes (502, mint_failed), exactly one audit event is emitted, the
// concurrency slot the handler held is released — checked directly via the
// semaphore's occupancy, not indirectly via a second request, since
// gcsLinkGlobalConcurrency is 16 and a second request succeeding would not
// by itself prove the first slot was released (15 others would still be
// free even if it leaked) — and no goroutine leak. If the mint ignored its
// ctx, cancellation would never unblock it and the second select below
// would fail the test after its own explicit timeout, rather than hanging
// until go test's own -timeout.
func TestGCSLink_MintBoundedByRequestDeadlineAndReleasesSlot(t *testing.T) {
	f := newGCSFixture(t)
	viewer, msg := gcsHappyPathFixture(t, f, "minttimeout", "gs://bkt/o.txt")

	// Recorded after the fixture (and its background services) are already
	// running, not before: the full test server starts several steady-state
	// goroutines of its own (control channel, scheduler, link services,
	// etc.), so a baseline taken before newGCSFixture would be far below the
	// server's actual steady state and never settle back to it.
	before := goroutineBaseline()

	// Swap the handler-level fake factory for the real production wiring, so
	// gcsObjectSourceFor's own ctx use is exercised.
	gen := newFakeGCSBlockingTokenGenerator()
	// If the mint were unbounded, GenerateAccessToken would block on
	// ctx.Done() forever; Unblock at cleanup forces it to return so the
	// goroutine below cannot outlive this test.
	t.Cleanup(gen.Unblock)
	f.srv.SetGCPTokenGenerator(gen)
	f.srv.gcsLinkSourceFactory = nil

	token, _, _, err := f.srv.userTokenService.GenerateTokenPair(
		viewer.ID, viewer.Email, viewer.DisplayName, viewer.Role, ClientTypeWeb,
	)
	require.NoError(t, err)
	reqCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, gcsRequestPath(msg.ID, "bkt", "o.txt"), nil).WithContext(reqCtx)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	start := time.Now()
	go func() {
		f.srv.Handler().ServeHTTP(rec, req)
		close(done)
	}()

	select {
	case <-gen.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the mint was never entered within 5s")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not complete within 5s of the request ctx being canceled: the mint is not bounded by the request ctx")
	}
	elapsed := time.Since(start)

	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	require.Less(t, elapsed, 2*time.Second, "the mint should return promptly once its ctx is canceled")
	require.Equal(t, 1, gen.mintCount())

	events := f.audit.all()
	require.Len(t, events, 1)
	require.Equal(t, GCSLinkReasonMintFailed, events[0].Reason)

	// The concurrency slot the request held must have been released. The
	// release is a single unconditional defer in handleGCSObject, shared by
	// every return path, so this checks the release mechanism itself rather
	// than anything specific to the mint-failure path.
	require.Equal(t, 0, len(f.srv.gcsLinkSem), "the concurrency slot held by the mint-failure request must be released")

	gen.Unblock()
	requireNoGoroutineLeak(t, before)
}

// TestGCSLink_RealSource_MintFailureSurfacesEagerly covers the production
// source-factory path specifically (gcsObjectSourceFor, not the fake
// factory TestGCSLink_MintFailure_502 uses): a mint failure from the real
// GCPTokenGenerator surfaces directly from gcsObjectSourceFor, before any
// GCS call.
func TestGCSLink_RealSource_MintFailureSurfacesEagerly(t *testing.T) {
	gen := &fakeGCSTokenGenerator{failMint: true}
	srv := &Server{gcpTokenGenerator: gen}

	_, release, err := srv.gcsObjectSourceFor(context.Background(), "sa@test.iam.gserviceaccount.com")
	release()
	require.Error(t, err)
	require.Equal(t, 1, gen.mintCount())
}

// TestGCSLink_WriteDeadlineBoundsStalledClientWrite proves the response
// write (step 13) is bounded by the same per-request deadline as everything
// else, not left to whatever the hub's server-wide, operator-configurable
// WriteTimeout happens to be (including 0, unbounded). It drives the real
// handler behind a real httptest.Server (an httptest.ResponseRecorder never
// blocks on Write, so it cannot exercise this at all) with a raw TCP client
// that reads only the response headers and then never reads the body, for
// an object large enough that the server's Write into the stalled
// connection must eventually block.
func TestGCSLink_WriteDeadlineBoundsStalledClientWrite(t *testing.T) {
	f := newGCSFixture(t)
	viewer, msg := gcsHappyPathFixture(t, f, "writedeadline", "gs://bkt/big.txt")

	body := bytes.Repeat([]byte("x"), 8*1024*1024) // under the 10 MiB cap, comfortably larger than default socket buffers
	f.source.putObject("bkt", "big.txt", &storage.ObjectAttrs{Size: int64(len(body)), Generation: 1}, body)
	f.srv.gcsLinkRequestDeadlineOverride = 300 * time.Millisecond

	hubTS := httptest.NewServer(f.srv.Handler())
	defer hubTS.Close()

	// Recorded after the fixture and hubTS (and their steady-state
	// goroutines: control channel, scheduler, link services, hubTS's accept
	// loop, etc.) are already running, not before — see the same note on
	// TestGCSLink_MintBoundedByRequestDeadlineAndReleasesSlot.
	before := goroutineBaseline()

	token, _, _, err := f.srv.userTokenService.GenerateTokenPair(
		viewer.ID, viewer.Email, viewer.DisplayName, viewer.Role, ClientTypeWeb,
	)
	require.NoError(t, err)

	conn, err := net.Dial("tcp", hubTS.Listener.Addr().String())
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	// A tiny receive buffer caps the effective TCP window well below the
	// object size regardless of how large the server's own send-side
	// buffers auto-tune to, so the server's Write reliably blocks on this
	// stalled connection rather than the whole body slipping into kernel
	// buffers unobserved.
	require.NoError(t, conn.(*net.TCPConn).SetReadBuffer(1024))

	req, err := http.NewRequest(http.MethodGet, hubTS.URL+gcsRequestPath(msg.ID, "bkt", "big.txt"), nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+token)
	require.NoError(t, req.Write(conn))

	// Read the status line and headers only — deliberately never read the
	// body, simulating a client that stops reading partway through.
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	start := time.Now()
	deadlineHit := make(chan struct{})
	go func() {
		for len(f.srv.gcsLinkSem) > 0 {
			time.Sleep(10 * time.Millisecond)
		}
		close(deadlineHit)
	}()
	select {
	case <-deadlineHit:
	case <-time.After(5 * time.Second):
		t.Fatal("the concurrency slot was not released within 5s: the stalled write is not bounded")
	}
	elapsed := time.Since(start)
	require.Less(t, elapsed, 3*time.Second, "the write should be bounded by the ~300ms request-deadline override, not the stalled connection")

	events := f.audit.all()
	require.Len(t, events, 1)
	require.Equal(t, "allow", events[0].Decision, "the response had already started (200, headers sent) by the time the write stalled")
	require.Equal(t, GCSLinkReasonUpstreamError, events[0].Reason, "a write that fails partway through is audited the same way a failed read partway through is")

	// Close the stalled connection explicitly (rather than waiting for the
	// deferred close at test end) so the server's per-connection goroutine
	// has a chance to unwind before the leak check below.
	_ = conn.Close()
	requireNoGoroutineLeak(t, before)
}

// goroutineLeakTolerance is added to a test's recorded baseline before
// requireNoGoroutineLeak compares against it. Goroutine counts are not
// perfectly deterministic even with nothing leaking: a GC assist worker or a
// runtime timer can transiently push the count a little above the pre-test
// baseline before settling. A real leak from an unbounded wait site holds
// its goroutine indefinitely on a channel receive or mutex that nothing
// unblocks, so it never settles within requireNoGoroutineLeak's 2s poll
// regardless of this tolerance — the tolerance absorbs unrelated churn
// without masking that case.
const goroutineLeakTolerance = 1

// goroutineBaseline returns a settled runtime.NumGoroutine() reading, for use
// as requireNoGoroutineLeak's before. These tests run in the same process as
// every other test in the package, back to back; a preceding test that tore
// down a full Server (background services: control channel, scheduler, link
// services, etc.) or an httptest.Server can still be unwinding its own
// goroutines when the next test starts, which would otherwise look like a
// baseline inflated by nothing this test did. Polling down to a stable
// reading (two consecutive equal reads, or giving up after 1s) avoids
// attributing that unrelated, in-progress teardown to this test's own
// goroutine count.
func goroutineBaseline() int {
	n := runtime.NumGoroutine()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		m := runtime.NumGoroutine()
		if m >= n {
			break
		}
		n = m
	}
	return n
}

// requireNoGoroutineLeak asserts the goroutine count has settled back to at
// most before+goroutineLeakTolerance within 2s, polling every 10ms. Call it
// after unblocking whatever blocking fake the test used (the generator,
// server handler, etc.) and after the request(s) depending on it have
// returned, with before recorded via goroutineBaseline() at the test's
// start, before any blocking fake was engaged.
func requireNoGoroutineLeak(t *testing.T, before int) {
	t.Helper()
	require.Eventually(t, func() bool {
		return runtime.NumGoroutine() <= before+goroutineLeakTolerance
	}, 2*time.Second, 10*time.Millisecond,
		"goroutine count did not settle back within tolerance of the baseline (%d, +%d): a wait site may have leaked a goroutine", before, goroutineLeakTolerance)
}

// gcsConcurrentSourceCalls fires n concurrent srv.gcsObjectSourceFor(ctx,
// email) calls, each with its own ctxTimeout-bounded context, and returns
// their errors in call order. It fails the test outright if the batch does
// not finish within an outer 5s deadline, so a regression that reintroduces
// an unbounded wait fails fast with a clear message instead of hanging
// until go test's own -timeout.
func gcsConcurrentSourceCalls(t *testing.T, srv *Server, email string, n int, ctxTimeout time.Duration) []error {
	t.Helper()
	results := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), ctxTimeout)
			defer cancel()
			_, release, err := srv.gcsObjectSourceFor(ctx, email)
			release()
			results[i] = err
		}(i)
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent gcsObjectSourceFor calls did not complete within 5s")
	}
	return results
}

// TestGCSLink_ConcurrentSameSARequestsDoNotSerialize proves concurrent
// requests for the same SA, against a token generator that never returns on
// its own, each return around their own ctx deadline instead of serializing
// behind one another (there is no per-SA cache or singleflight dedup —
// each request mints its own token independently, so the generator sees
// one call per request, not one shared call). The
// maxInFlight assertion is what actually proves "not serializing": with all
// 4 calls overlapping in time, a mutex around the mint would still let
// every call return by its own ctx deadline and still produce 4 total
// calls, just one at a time — only the high-water mark distinguishes true
// concurrency from disguised serialization.
func TestGCSLink_ConcurrentSameSARequestsDoNotSerialize(t *testing.T) {
	before := goroutineBaseline()
	const email = "sa-concurrent@test.iam.gserviceaccount.com"
	gen := newFakeGCSBlockingTokenGenerator()
	t.Cleanup(gen.Unblock)
	srv := &Server{gcpTokenGenerator: gen}

	start := time.Now()
	results := gcsConcurrentSourceCalls(t, srv, email, 4, 300*time.Millisecond)
	elapsed := time.Since(start)

	require.Less(t, elapsed, 2*time.Second, "every call should return around its own ~300ms ctx deadline, not serialize behind the others")
	for i, err := range results {
		require.ErrorIs(t, err, context.DeadlineExceeded, "call %d", i)
	}
	require.Equal(t, 4, gen.mintCount(), "each request mints its own token independently; nothing is deduped")
	require.Equal(t, 4, gen.maxInFlight(), "all 4 mints must be in progress at once, not serialized behind one another")

	gen.Unblock()
	requireNoGoroutineLeak(t, before)
}

// ---------------------------------------------------------------------------
// Allowed fetches (project topic, agent<->user DM).
// ---------------------------------------------------------------------------

func TestGCSLink_Allowed_ProjectTopic(t *testing.T) {
	f := newGCSFixture(t)
	viewer, msg := gcsHappyPathFixture(t, f, "topic-ok", "gs://bkt/dir/o.txt")
	f.source.putObject("bkt", "dir/o.txt", &storage.ObjectAttrs{Size: 11, Generation: 42}, []byte("hello world"))

	rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "dir/o.txt"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "hello world", rec.Body.String())
	require.Equal(t, "text/plain; charset=utf-8", rec.Header().Get("Content-Type"))
	require.Equal(t, "11", rec.Header().Get("Content-Length"))
	require.Equal(t, `attachment; filename*=UTF-8''o.txt`, rec.Header().Get("Content-Disposition"))
	require.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	require.Equal(t, untrustedContentSandboxCSP, rec.Header().Get("Content-Security-Policy"))
	require.Equal(t, "private, no-store", rec.Header().Get("Cache-Control"))
	require.Equal(t, "42", rec.Header().Get("X-Scion-Gcs-Generation"))

	call, ok := f.gen.lastCall()
	require.True(t, ok, "expected a mint call")
	require.Equal(t, "sa-a-topic-ok@test.iam.gserviceaccount.com", call.email)
	require.Equal(t, []string{"https://www.googleapis.com/auth/devstorage.read_only"}, call.scopes)
}

func TestGCSLink_Allowed_AgentUserDM(t *testing.T) {
	f := newGCSFixture(t)
	project := gcsTestProject(t, f, "dm-project")
	sa := gcsTestSA(t, f.store, project, "dm-sa", "sa-dm@test.iam.gserviceaccount.com")
	agent := gcsTestAgent(t, f.store, project, "dm-agent", gcsAssignedIdentity(sa))
	viewer := gcsTestUser(t, f.store, "dm-viewer")
	conv := gcsTestDM(t, f.store, "dm-conv", "dm:agent:"+agent.ID+":user:"+viewer.ID)
	msg := gcsTestMessage(t, f.store, gcsMessageSpec{
		seed: "dm-msg", projectID: project.ID, sender: "agent:" + agent.Slug, senderID: agent.ID,
		body: "gs://bkt/note.txt", agentID: agent.ID, conversationID: conv.ID,
	})
	f.source.putObject("bkt", "note.txt", &storage.ObjectAttrs{Size: 2, Generation: 1}, []byte("hi"))

	rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "note.txt"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "hi", rec.Body.String())
}

// ---------------------------------------------------------------------------
// Identical 404 across every deny path (each test below compares via
// JSONEq against the same canonical gcsExpected404Body; see
// TestGCSLink_Enumeration_ByteIdenticalAcrossDenyPaths further down for the
// actual byte-for-byte comparison across paths), zero GCS calls, zero
// mints. Each test below is a positive-control fixture (gcsHappyPathFixture
// or a close variant) with exactly one variable changed.
// ---------------------------------------------------------------------------

const gcsExpected404Body = `{"error":{"code":"not_found","message":"Object not found"}}`

func TestGCSLink_Deny_MessageIDUnknown(t *testing.T) {
	f := newGCSFixture(t)
	viewer, _ := gcsHappyPathFixture(t, f, "unknown-msg", "gs://bkt/o.txt")

	rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(tid("does-not-exist"), "bkt", "o.txt"), nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.JSONEq(t, gcsExpected404Body, rec.Body.String())
	attrsCalls, openCalls := f.source.counts()
	require.Zero(t, attrsCalls)
	require.Zero(t, openCalls)
	require.Zero(t, f.gen.mintCount())
}

func TestGCSLink_Deny_MessageSoftDeleted(t *testing.T) {
	f := newGCSFixture(t)
	viewer, msg := gcsHappyPathFixture(t, f, "soft-del", "gs://bkt/o.txt")
	require.NoError(t, f.srv.webChatStore.SetMessageDeleted(context.Background(), msg.ID, time.Now()))

	rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "o.txt"), nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.JSONEq(t, gcsExpected404Body, rec.Body.String())
	attrsCalls, openCalls := f.source.counts()
	require.Zero(t, attrsCalls)
	require.Zero(t, openCalls)
	require.Zero(t, f.gen.mintCount())

	events := f.audit.all()
	require.Len(t, events, 1)
	require.Equal(t, GCSLinkReasonMessageNotReadable, events[0].Reason)
}

// gcsFailingWebChatStore wraps a WebChatStore, injecting a store fault from
// GetMessageExt, distinct from its ordinary "no row" (nil, nil) return — so
// a test can prove canUserReadMessage fails closed on a genuine error
// instead of treating it the same as "not soft-deleted."
type gcsFailingWebChatStore struct {
	WebChatStore
	err error
}

func (w *gcsFailingWebChatStore) GetMessageExt(context.Context, string) (*WebChatMessageExt, error) {
	return nil, w.err
}

// TestGCSLink_Deny_MessageExtLookupFails isolates canUserReadMessage's
// GetMessageExt error check specifically: every other guard passes cleanly
// (same fixture TestGCSLink_Deny_MessageSoftDeleted's positive path uses,
// just without ever soft-deleting the message), so only a genuine store
// fault from the soft-delete lookup denies the fetch.
func TestGCSLink_Deny_MessageExtLookupFails(t *testing.T) {
	f := newGCSFixture(t)
	viewer, msg := gcsHappyPathFixture(t, f, "extlookupfail", "gs://bkt/o.txt")
	f.source.putObject("bkt", "o.txt", &storage.ObjectAttrs{Size: 2, Generation: 1}, []byte("hi"))
	f.srv.webChatStore = &gcsFailingWebChatStore{WebChatStore: f.srv.webChatStore, err: errors.New("gcs test: simulated GetMessageExt store fault")}

	rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "o.txt"), nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.JSONEq(t, gcsExpected404Body, rec.Body.String())
	attrsCalls, openCalls := f.source.counts()
	require.Zero(t, attrsCalls)
	require.Zero(t, openCalls)
	require.Zero(t, f.gen.mintCount())

	events := f.audit.all()
	require.Len(t, events, 1)
	require.Equal(t, GCSLinkReasonMessageNotReadable, events[0].Reason)
}

// gcsFailingConversationStore wraps a store.Store, injecting a store fault
// from GetConversation for one specific conversation id, so a test can prove
// canUserReadMessage's own GetConversation check fails closed on a genuine
// error rather than surfacing it or treating it as some other deny reason.
type gcsFailingConversationStore struct {
	store.Store
	convID string
	err    error
}

func (f *gcsFailingConversationStore) GetConversation(ctx context.Context, id string) (*store.Conversation, error) {
	if id == f.convID {
		return nil, f.err
	}
	return f.Store.GetConversation(ctx, id)
}

// TestGCSLink_Deny_ConversationLookupStoreFault drives canUserReadMessage's
// GetConversation check with a genuine store fault, as opposed to
// TestGCSLink_Deny_ConversationLookupFails's ordinary "not found": both must
// deny with the uniform 404, no GCS or mint calls, and the
// message_not_readable audit reason. (Only the fault case is logged; this
// test does not assert logging.)
func TestGCSLink_Deny_ConversationLookupStoreFault(t *testing.T) {
	f := newGCSFixture(t)
	viewer, msg := gcsHappyPathFixture(t, f, "convlookupfail", "gs://bkt/o.txt")
	f.source.putObject("bkt", "o.txt", &storage.ObjectAttrs{Size: 2, Generation: 1}, []byte("hi"))
	f.srv.store = &gcsFailingConversationStore{
		Store: f.srv.store, convID: msg.ConversationID,
		err: errors.New("gcs test: simulated GetConversation store fault"),
	}

	rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "o.txt"), nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.JSONEq(t, gcsExpected404Body, rec.Body.String())
	attrsCalls, openCalls := f.source.counts()
	require.Zero(t, attrsCalls)
	require.Zero(t, openCalls)
	require.Zero(t, f.gen.mintCount())

	events := f.audit.all()
	require.Len(t, events, 1)
	require.Equal(t, GCSLinkReasonMessageNotReadable, events[0].Reason)
}

// TestGCSLink_Deny_GroupConversationLookupFails checks that a genuine store
// fault inside the group-read check denies with the uniform 404, end to end
// through the real gcs endpoint, not just at canReadGroupConversation's own
// unit level (see TestGCSLink_CanReadGroupConversation for that).
func TestGCSLink_Deny_GroupConversationLookupFails(t *testing.T) {
	f := newGCSFixture(t)
	viewer, msg := gcsHappyPathFixture(t, f, "groupconvfail", "gs://bkt/o.txt")
	f.source.putObject("bkt", "o.txt", &storage.ObjectAttrs{Size: 2, Generation: 1}, []byte("hi"))
	f.srv.store = &gcsFailingProjectStore{
		Store: f.srv.store,
		err:   errors.New("gcs test: simulated GetProject store fault"),
	}

	rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "o.txt"), nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.JSONEq(t, gcsExpected404Body, rec.Body.String())
	attrsCalls, openCalls := f.source.counts()
	require.Zero(t, attrsCalls)
	require.Zero(t, openCalls)
	require.Zero(t, f.gen.mintCount())

	events := f.audit.all()
	require.Len(t, events, 1)
	require.Equal(t, GCSLinkReasonMessageNotReadable, events[0].Reason)
}

func TestGCSLink_Deny_ViewerWithoutProjectRead(t *testing.T) {
	f := newGCSFixture(t)
	project := gcsTestProject(t, f, "noread-project")
	sa := gcsTestSA(t, f.store, project, "noread-sa", "sa-noread@test.iam.gserviceaccount.com")
	agent := gcsTestAgent(t, f.store, project, "noread-agent", gcsAssignedIdentity(sa))
	conv := gcsTestTopic(t, f.store, project, "noread-topic")
	msg := gcsTestMessage(t, f.store, gcsMessageSpec{
		seed: "noread-msg", projectID: project.ID, sender: "agent:" + agent.Slug, senderID: agent.ID,
		body: "gs://bkt/o.txt", agentID: agent.ID, conversationID: conv.ID,
	})
	// Positive control: an equivalent viewer WITH project.read succeeds.
	memberViewer := gcsTestUser(t, f.store, "noread-member")
	addProjectMemberWithRole(t, f.store, project, memberViewer.ID, store.GroupMemberRoleMember)
	f.source.putObject("bkt", "o.txt", &storage.ObjectAttrs{Size: 2, Generation: 1}, []byte("hi"))
	okRec := doRequestAsUser(t, f.srv, memberViewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "o.txt"), nil)
	require.Equal(t, http.StatusOK, okRec.Code, okRec.Body.String())

	// The outsider (no project membership at all) is denied.
	outsider := gcsTestUser(t, f.store, "noread-outsider")
	before := len(f.audit.all())
	rec := doRequestAsUser(t, f.srv, outsider, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "o.txt"), nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.JSONEq(t, gcsExpected404Body, rec.Body.String())

	events := f.audit.all()
	require.Len(t, events, before+1)
	require.Equal(t, GCSLinkReasonMessageNotReadable, events[before].Reason)
}

func TestGCSLink_Deny_ViewerNotDMParticipant(t *testing.T) {
	f := newGCSFixture(t)
	project := gcsTestProject(t, f, "dmnp-project")
	sa := gcsTestSA(t, f.store, project, "dmnp-sa", "sa-dmnp@test.iam.gserviceaccount.com")
	agent := gcsTestAgent(t, f.store, project, "dmnp-agent", gcsAssignedIdentity(sa))
	participant := gcsTestUser(t, f.store, "dmnp-participant")
	conv := gcsTestDM(t, f.store, "dmnp-conv", "dm:agent:"+agent.ID+":user:"+participant.ID)
	msg := gcsTestMessage(t, f.store, gcsMessageSpec{
		seed: "dmnp-msg", projectID: project.ID, sender: "agent:" + agent.Slug, senderID: agent.ID,
		body: "gs://bkt/o.txt", agentID: agent.ID, conversationID: conv.ID,
	})
	f.source.putObject("bkt", "o.txt", &storage.ObjectAttrs{Size: 2, Generation: 1}, []byte("hi"))

	// Positive control: the real participant succeeds.
	okRec := doRequestAsUser(t, f.srv, participant, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "o.txt"), nil)
	require.Equal(t, http.StatusOK, okRec.Code, okRec.Body.String())

	outsider := gcsTestUser(t, f.store, "dmnp-outsider")
	before := len(f.audit.all())
	rec := doRequestAsUser(t, f.srv, outsider, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "o.txt"), nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.JSONEq(t, gcsExpected404Body, rec.Body.String())

	events := f.audit.all()
	require.Len(t, events, before+1)
	require.Equal(t, GCSLinkReasonMessageNotReadable, events[before].Reason)
}

func TestGCSLink_Deny_UserSentMessage(t *testing.T) {
	f := newGCSFixture(t)
	project := gcsTestProject(t, f, "usersent-project")
	conv := gcsTestTopic(t, f.store, project, "usersent-topic")
	sender := gcsTestUser(t, f.store, "usersent-sender")
	addProjectMemberWithRole(t, f.store, project, sender.ID, store.GroupMemberRoleMember)
	msg := gcsTestMessage(t, f.store, gcsMessageSpec{
		seed: "usersent-msg", projectID: project.ID, sender: "user:" + sender.Email, senderID: sender.ID,
		body: "gs://bkt/o.txt", conversationID: conv.ID,
	})

	// The viewer can read this message (project member); the expected deny
	// reason, asserted below, is sender_not_agent.
	rec := doRequestAsUser(t, f.srv, sender, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "o.txt"), nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.JSONEq(t, gcsExpected404Body, rec.Body.String())
	attrsCalls, openCalls := f.source.counts()
	require.Zero(t, attrsCalls)
	require.Zero(t, openCalls)
	require.Zero(t, f.gen.mintCount())

	events := f.audit.all()
	require.Len(t, events, 1)
	require.Equal(t, GCSLinkReasonSenderNotAgent, events[0].Reason)
}

func TestGCSLink_Deny_UserUserDM(t *testing.T) {
	f := newGCSFixture(t)
	project := gcsTestProject(t, f, "uu-project")
	alice := gcsTestUser(t, f.store, "uu-alice")
	bob := gcsTestUser(t, f.store, "uu-bob")
	conv := gcsTestDM(t, f.store, "uu-conv", "dm:user:"+alice.ID+":user:"+bob.ID)
	msg := gcsTestMessage(t, f.store, gcsMessageSpec{
		seed: "uu-msg", projectID: project.ID, sender: "user:" + alice.Email, senderID: alice.ID,
		body: "gs://bkt/o.txt", conversationID: conv.ID,
	})

	// bob is a DM participant and can read the message; the expected deny
	// reason, asserted below, is sender_not_agent.
	rec := doRequestAsUser(t, f.srv, bob, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "o.txt"), nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.JSONEq(t, gcsExpected404Body, rec.Body.String())
	attrsCalls, openCalls := f.source.counts()
	require.Zero(t, attrsCalls)
	require.Zero(t, openCalls)
	require.Zero(t, f.gen.mintCount())

	events := f.audit.all()
	require.Len(t, events, 1)
	require.Equal(t, GCSLinkReasonSenderNotAgent, events[0].Reason)
}

func TestGCSLink_Deny_AgentPassthroughMode(t *testing.T) {
	f := newGCSFixture(t)
	project := gcsTestProject(t, f, "passthrough-project")
	agent := gcsTestAgent(t, f.store, project, "passthrough-agent", &store.GCPIdentityConfig{MetadataMode: store.GCPMetadataModePassthrough})
	conv := gcsTestTopic(t, f.store, project, "passthrough-topic")
	msg := gcsTestMessage(t, f.store, gcsMessageSpec{
		seed: "passthrough-msg", projectID: project.ID, sender: "agent:" + agent.Slug, senderID: agent.ID,
		body: "gs://bkt/o.txt", agentID: agent.ID, conversationID: conv.ID,
	})
	viewer := gcsTestUser(t, f.store, "passthrough-viewer")
	addProjectMemberWithRole(t, f.store, project, viewer.ID, store.GroupMemberRoleMember)

	rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "o.txt"), nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.JSONEq(t, gcsExpected404Body, rec.Body.String())
	require.Zero(t, f.gen.mintCount())

	events := f.audit.all()
	require.Len(t, events, 1)
	require.Equal(t, GCSLinkReasonNoSA, events[0].Reason)
}

// TestGCSLink_Deny_PassthroughWithStaleSAFields isolates the MetadataMode
// check specifically: the SA id/email fields are still populated (as they
// might be if an agent was switched from assign to passthrough without
// clearing them), so only the mode check itself denies the fetch.
func TestGCSLink_Deny_PassthroughWithStaleSAFields(t *testing.T) {
	f := newGCSFixture(t)
	project := gcsTestProject(t, f, "stalesa-project")
	sa := gcsTestSA(t, f.store, project, "stalesa-sa", "sa-stalesa@test.iam.gserviceaccount.com")
	agent := gcsTestAgent(t, f.store, project, "stalesa-agent", &store.GCPIdentityConfig{
		MetadataMode: store.GCPMetadataModePassthrough, ServiceAccountID: sa.ID, ServiceAccountEmail: sa.Email,
	})
	conv := gcsTestTopic(t, f.store, project, "stalesa-topic")
	msg := gcsTestMessage(t, f.store, gcsMessageSpec{
		seed: "stalesa-msg", projectID: project.ID, sender: "agent:" + agent.Slug, senderID: agent.ID,
		body: "gs://bkt/o.txt", agentID: agent.ID, conversationID: conv.ID,
	})
	viewer := gcsTestUser(t, f.store, "stalesa-viewer")
	addProjectMemberWithRole(t, f.store, project, viewer.ID, store.GroupMemberRoleMember)

	rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "o.txt"), nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.JSONEq(t, gcsExpected404Body, rec.Body.String())
	require.Zero(t, f.gen.mintCount())

	events := f.audit.all()
	require.Len(t, events, 1)
	require.Equal(t, GCSLinkReasonNoSA, events[0].Reason)
}

func TestGCSLink_Deny_AgentNoGCPIdentity(t *testing.T) {
	f := newGCSFixture(t)
	project := gcsTestProject(t, f, "noidentity-project")
	agent := gcsTestAgent(t, f.store, project, "noidentity-agent", nil)
	conv := gcsTestTopic(t, f.store, project, "noidentity-topic")
	msg := gcsTestMessage(t, f.store, gcsMessageSpec{
		seed: "noidentity-msg", projectID: project.ID, sender: "agent:" + agent.Slug, senderID: agent.ID,
		body: "gs://bkt/o.txt", agentID: agent.ID, conversationID: conv.ID,
	})
	viewer := gcsTestUser(t, f.store, "noidentity-viewer")
	addProjectMemberWithRole(t, f.store, project, viewer.ID, store.GroupMemberRoleMember)

	rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "o.txt"), nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.JSONEq(t, gcsExpected404Body, rec.Body.String())
	require.Zero(t, f.gen.mintCount())

	events := f.audit.all()
	require.Len(t, events, 1)
	require.Equal(t, GCSLinkReasonNoSA, events[0].Reason)
}

func TestGCSLink_Deny_SARecordDeleted(t *testing.T) {
	f := newGCSFixture(t)
	viewer, msg := gcsHappyPathFixture(t, f, "sadeleted", "gs://bkt/o.txt")
	require.NoError(t, f.store.DeleteGCPServiceAccount(context.Background(), tid("sadeleted-sa-a")))

	rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "o.txt"), nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.JSONEq(t, gcsExpected404Body, rec.Body.String())
	require.Zero(t, f.gen.mintCount())

	events := f.audit.all()
	require.Len(t, events, 1)
	require.Equal(t, GCSLinkReasonNoSA, events[0].Reason)
}

func TestGCSLink_Deny_AgentDeleted(t *testing.T) {
	f := newGCSFixture(t)
	viewer, msg := gcsHappyPathFixture(t, f, "agentdeleted", "gs://bkt/o.txt")
	require.NoError(t, f.store.DeleteAgent(context.Background(), tid("agentdeleted-agent")))

	rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "o.txt"), nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.JSONEq(t, gcsExpected404Body, rec.Body.String())
	require.Zero(t, f.gen.mintCount())

	// The agent record is gone, so resolveSenderAgent's own GetAgent lookup
	// fails before SA resolution is ever reached — sender_not_agent, not
	// no_sa.
	events := f.audit.all()
	require.Len(t, events, 1)
	require.Equal(t, GCSLinkReasonSenderNotAgent, events[0].Reason)
}

func TestGCSLink_Deny_ForgedSender_BrokerInboundEmptySenderID(t *testing.T) {
	f := newGCSFixture(t)
	project := gcsTestProject(t, f, "forged-project")
	conv := gcsTestTopic(t, f.store, project, "forged-topic")
	viewer := gcsTestUser(t, f.store, "forged-viewer")
	addProjectMemberWithRole(t, f.store, project, viewer.ID, store.GroupMemberRoleMember)
	// Broker-inbound shape: Sender claims "agent:x" but SenderID is empty,
	// exactly as an unresolved broker row looks (handlers_broker_inbound.go).
	msg := gcsTestMessage(t, f.store, gcsMessageSpec{
		seed: "forged-msg", projectID: project.ID, sender: "agent:x", senderID: "",
		body: "gs://bkt/o.txt", conversationID: conv.ID,
	})

	rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "o.txt"), nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.JSONEq(t, gcsExpected404Body, rec.Body.String())
	require.Zero(t, f.gen.mintCount())
}

func TestGCSLink_Deny_SenderIDSlugMismatch(t *testing.T) {
	f := newGCSFixture(t)
	project := gcsTestProject(t, f, "slugmismatch-project")
	sa := gcsTestSA(t, f.store, project, "slugmismatch-sa", "sa-slugmismatch@test.iam.gserviceaccount.com")
	agent := gcsTestAgent(t, f.store, project, "slugmismatch-agent", gcsAssignedIdentity(sa))
	conv := gcsTestTopic(t, f.store, project, "slugmismatch-topic")
	viewer := gcsTestUser(t, f.store, "slugmismatch-viewer")
	addProjectMemberWithRole(t, f.store, project, viewer.ID, store.GroupMemberRoleMember)
	// SenderID resolves to a real agent, but the claimed Sender slug doesn't match it.
	msg := gcsTestMessage(t, f.store, gcsMessageSpec{
		seed: "slugmismatch-msg", projectID: project.ID, sender: "agent:someone-else", senderID: agent.ID,
		body: "gs://bkt/o.txt", agentID: agent.ID, conversationID: conv.ID,
	})

	rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "o.txt"), nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.JSONEq(t, gcsExpected404Body, rec.Body.String())
	require.Zero(t, f.gen.mintCount())
}

// TestGCSLink_Deny_SenderNoAgentPrefixAtAll isolates the "agent:" prefix
// check specifically: SenderID resolves to a real, live agent (so a lookup
// failure can't be what denies this), but Sender itself was never
// agent-shaped at all.
func TestGCSLink_Deny_SenderNoAgentPrefixAtAll(t *testing.T) {
	f := newGCSFixture(t)
	project := gcsTestProject(t, f, "noprefix-project")
	sa := gcsTestSA(t, f.store, project, "noprefix-sa", "sa-noprefix@test.iam.gserviceaccount.com")
	agent := gcsTestAgent(t, f.store, project, "noprefix-agent", gcsAssignedIdentity(sa))
	conv := gcsTestTopic(t, f.store, project, "noprefix-topic")
	viewer := gcsTestUser(t, f.store, "noprefix-viewer")
	addProjectMemberWithRole(t, f.store, project, viewer.ID, store.GroupMemberRoleMember)
	msg := gcsTestMessage(t, f.store, gcsMessageSpec{
		seed: "noprefix-msg", projectID: project.ID, sender: "not-agent-shaped", senderID: agent.ID,
		body: "gs://bkt/o.txt", agentID: agent.ID, conversationID: conv.ID,
	})

	rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "o.txt"), nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.JSONEq(t, gcsExpected404Body, rec.Body.String())
	require.Zero(t, f.gen.mintCount())
}

func TestGCSLink_Deny_EmptyConversationID(t *testing.T) {
	f := newGCSFixture(t)
	project := gcsTestProject(t, f, "emptyconv-project")
	sa := gcsTestSA(t, f.store, project, "emptyconv-sa", "sa-emptyconv@test.iam.gserviceaccount.com")
	agent := gcsTestAgent(t, f.store, project, "emptyconv-agent", gcsAssignedIdentity(sa))
	viewer := gcsTestUser(t, f.store, "emptyconv-viewer")
	addProjectMemberWithRole(t, f.store, project, viewer.ID, store.GroupMemberRoleMember)
	msg := gcsTestMessage(t, f.store, gcsMessageSpec{
		seed: "emptyconv-msg", projectID: project.ID, sender: "agent:" + agent.Slug, senderID: agent.ID,
		body: "gs://bkt/o.txt", agentID: agent.ID, conversationID: "",
	})

	rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "o.txt"), nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.JSONEq(t, gcsExpected404Body, rec.Body.String())
	require.Zero(t, f.gen.mintCount())
}

func TestGCSLink_Deny_AgentTokenCaller(t *testing.T) {
	f := newGCSFixture(t)
	_, msg := gcsHappyPathFixture(t, f, "agenttoken", "gs://bkt/o.txt")
	project := gcsTestProject(t, f, "agenttoken-caller-project")
	callerAgent := gcsTestAgent(t, f.store, project, "agenttoken-caller", nil)
	token, err := f.srv.agentTokenService.GenerateAgentToken(callerAgent.ID, project.ID, nil, nil)
	require.NoError(t, err)

	rec := doRequestWithAgentToken(t, f.srv, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "o.txt"), nil, token)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.JSONEq(t, gcsExpected404Body, rec.Body.String())
	require.Zero(t, f.gen.mintCount())

	events := f.audit.all()
	require.Len(t, events, 1)
	require.Equal(t, GCSLinkReasonNotUser, events[0].Reason)
}

// ---------------------------------------------------------------------------
// URI-in-body boundary, with a positive control per row.
// ---------------------------------------------------------------------------

func TestGCSLink_URIBoundary(t *testing.T) {
	f := newGCSFixture(t)
	project := gcsTestProject(t, f, "boundary-project")
	sa := gcsTestSA(t, f.store, project, "boundary-sa", "sa-boundary@test.iam.gserviceaccount.com")
	agent := gcsTestAgent(t, f.store, project, "boundary-agent", gcsAssignedIdentity(sa))
	conv := gcsTestTopic(t, f.store, project, "boundary-topic")
	viewer := gcsTestUser(t, f.store, "boundary-viewer")
	addProjectMemberWithRole(t, f.store, project, viewer.ID, store.GroupMemberRoleMember)
	msg := gcsTestMessage(t, f.store, gcsMessageSpec{
		seed: "boundary-msg", projectID: project.ID, sender: "agent:" + agent.Slug, senderID: agent.ID,
		body: "gs://bkt/abc.md", agentID: agent.ID, conversationID: conv.ID,
	})
	f.source.putObject("bkt", "abc.md", &storage.ObjectAttrs{Size: 1, Generation: 1}, []byte("x"))
	// Also register every prefix as a real (fake-source) object, with
	// distinguishable content. If bodyReferencesGCSURI's boundary check were
	// ever bypassed, the request would reach Attrs/Open and come back 200
	// with this canary body instead of the uniform 404 — the byte-identical
	// 404 alone can't distinguish "denied for the right reason" from "denied
	// because the fake object doesn't exist", so without this a boundary
	// regression could slip through undetected.
	for _, object := range []string{"a", "abc", "abc.m"} {
		f.source.putObject("bkt", object, &storage.ObjectAttrs{Size: 6, Generation: 1}, []byte("canary"))
	}

	// Positive control: the exact posted object is allowed.
	okRec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "abc.md"), nil)
	require.Equal(t, http.StatusOK, okRec.Code, okRec.Body.String())

	for _, object := range []string{"a", "abc", "abc.m"} {
		t.Run("prefix_"+object, func(t *testing.T) {
			before := len(f.audit.all())
			rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", object), nil)
			require.Equal(t, http.StatusNotFound, rec.Code)
			require.JSONEq(t, gcsExpected404Body, rec.Body.String())
			require.NotContains(t, rec.Body.String(), "canary")

			events := f.audit.all()
			require.Len(t, events, before+1)
			require.Equal(t, GCSLinkReasonURINotInBody, events[before].Reason)
		})
	}

	xgsMsg := gcsTestMessage(t, f.store, gcsMessageSpec{
		seed: "boundary-xgs-msg", projectID: project.ID, sender: "agent:" + agent.Slug, senderID: agent.ID,
		body: "xgs://bkt/o", agentID: agent.ID, conversationID: conv.ID,
	})
	f.source.putObject("bkt", "o", &storage.ObjectAttrs{Size: 6, Generation: 1}, []byte("canary"))
	rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(xgsMsg.ID, "bkt", "o"), nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.JSONEq(t, gcsExpected404Body, rec.Body.String())

	parenMsg := gcsTestMessage(t, f.store, gcsMessageSpec{
		seed: "boundary-paren-msg", projectID: project.ID, sender: "agent:" + agent.Slug, senderID: agent.ID,
		body: "(gs://bkt/o.md).", agentID: agent.ID, conversationID: conv.ID,
	})
	f.source.putObject("bkt", "o.md", &storage.ObjectAttrs{Size: 1, Generation: 1}, []byte("x"))
	parenRec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(parenMsg.ID, "bkt", "o.md"), nil)
	require.Equal(t, http.StatusOK, parenRec.Code, parenRec.Body.String())
}

// TestGCSLink_URIBoundary_TrailingFinalCharRun isolates the object
// extraction's handling of a trailing final-character run specifically: a
// posted object ending in a `~+=@%` run is the whole extraction, not a
// boundary — the full object is allowed, and the truncated form is denied
// with a canary present to prove it is denied by the boundary check, not
// because the object merely does not exist.
func TestGCSLink_URIBoundary_TrailingFinalCharRun(t *testing.T) {
	f := newGCSFixture(t)
	project := gcsTestProject(t, f, "trailingfinal-project")
	sa := gcsTestSA(t, f.store, project, "trailingfinal-sa", "sa-trailingfinal@test.iam.gserviceaccount.com")
	agent := gcsTestAgent(t, f.store, project, "trailingfinal-agent", gcsAssignedIdentity(sa))
	conv := gcsTestTopic(t, f.store, project, "trailingfinal-topic")
	viewer := gcsTestUser(t, f.store, "trailingfinal-viewer")
	addProjectMemberWithRole(t, f.store, project, viewer.ID, store.GroupMemberRoleMember)

	for _, c := range []struct {
		name              string
		posted, truncated string
	}{
		{"tilde", "secret~", "secret"},
		{"plus", "data+", "data"},
		{"double-equals", "key==", "key"},
		{"percent", "report%", "report"},
		{"at", "a@", "a"},
	} {
		t.Run(c.name, func(t *testing.T) {
			msg := gcsTestMessage(t, f.store, gcsMessageSpec{
				seed: "trailingfinal-" + c.name, projectID: project.ID, sender: "agent:" + agent.Slug, senderID: agent.ID,
				body: "gs://bkt/" + c.posted, agentID: agent.ID, conversationID: conv.ID,
			})
			f.source.putObject("bkt", c.posted, &storage.ObjectAttrs{Size: 1, Generation: 1}, []byte("x"))
			f.source.putObject("bkt", c.truncated, &storage.ObjectAttrs{Size: 6, Generation: 1}, []byte("canary"))

			okRec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", c.posted), nil)
			require.Equal(t, http.StatusOK, okRec.Code, okRec.Body.String())

			truncRec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", c.truncated), nil)
			require.Equal(t, http.StatusNotFound, truncRec.Code)
			require.JSONEq(t, gcsExpected404Body, truncRec.Body.String())
			require.NotContains(t, truncRec.Body.String(), "canary")
		})
	}
}

// TestGCSLink_URIBoundary_FragmentContinuation isolates the continuation
// rule: a `#`, `?` or `&` immediately after the object,
// followed by further non-whitespace text, denies the whole occurrence —
// not just the truncated object up to that character — with a canary
// present to prove the denial is the continuation rule, not a missing
// object.
func TestGCSLink_URIBoundary_FragmentContinuation(t *testing.T) {
	f := newGCSFixture(t)
	project := gcsTestProject(t, f, "fragcont-project")
	sa := gcsTestSA(t, f.store, project, "fragcont-sa", "sa-fragcont@test.iam.gserviceaccount.com")
	agent := gcsTestAgent(t, f.store, project, "fragcont-agent", gcsAssignedIdentity(sa))
	conv := gcsTestTopic(t, f.store, project, "fragcont-topic")
	viewer := gcsTestUser(t, f.store, "fragcont-viewer")
	addProjectMemberWithRole(t, f.store, project, viewer.ID, store.GroupMemberRoleMember)

	for _, c := range []struct {
		name, body string
	}{
		{"hash", "gs://bkt/a#frag"},
		{"query", "gs://bkt/a?x=1"},
		{"amp", "gs://bkt/a&b"},
	} {
		t.Run(c.name, func(t *testing.T) {
			msg := gcsTestMessage(t, f.store, gcsMessageSpec{
				seed: "fragcont-" + c.name, projectID: project.ID, sender: "agent:" + agent.Slug, senderID: agent.ID,
				body: c.body, agentID: agent.ID, conversationID: conv.ID,
			})
			f.source.putObject("bkt", "a", &storage.ObjectAttrs{Size: 6, Generation: 1}, []byte("canary"))

			rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "a"), nil)
			require.Equal(t, http.StatusNotFound, rec.Code)
			require.JSONEq(t, gcsExpected404Body, rec.Body.String())
			require.NotContains(t, rec.Body.String(), "canary")
		})
	}
}

// ---------------------------------------------------------------------------
// Extra or duplicate query params.
// ---------------------------------------------------------------------------

func TestGCSLink_ExtraOrDuplicateParams(t *testing.T) {
	f := newGCSFixture(t)
	viewer, msg := gcsHappyPathFixture(t, f, "params", "gs://bkt/o.txt")

	cases := map[string]string{
		"extra_agent_param": gcsRequestPath(msg.ID, "bkt", "o.txt") + "&agent=someone",
		"extra_sa_param":    gcsRequestPath(msg.ID, "bkt", "o.txt") + "&sa=sa-a@test.iam.gserviceaccount.com",
		"extra_email_param": gcsRequestPath(msg.ID, "bkt", "o.txt") + "&email=sa-a@test.iam.gserviceaccount.com",
		"duplicate_object":  gcsRequestPath(msg.ID, "bkt", "o.txt") + "&object=other.txt",
		"duplicate_message": gcsRequestPath(msg.ID, "bkt", "o.txt") + "&message=" + tid("other-msg"),
		"missing_object":    "/api/v1/gcs/object?" + url.Values{"message": {msg.ID}, "bucket": {"bkt"}}.Encode(),
		// Exactly three keys, but one is misnamed rather than extra — this is
		// a different code path (the switch's default case) than the
		// too-many-keys check above.
		"wrong_key_name": "/api/v1/gcs/object?" + url.Values{"message": {msg.ID}, "bucket": {"bkt"}, "evil": {"o.txt"}}.Encode(),
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			before := len(f.audit.all())
			rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, path, nil)
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			var body ErrorResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			require.Equal(t, ErrCodeInvalidRequest, body.Error.Code)

			events := f.audit.all()
			require.Len(t, events, before+1)
			require.Equal(t, GCSLinkReasonBadRequest, events[before].Reason)
		})
	}
	attrsCalls, openCalls := f.source.counts()
	require.Zero(t, attrsCalls)
	require.Zero(t, openCalls)
	require.Zero(t, f.gen.mintCount())
}

// ---------------------------------------------------------------------------
// SA reassignment and unassignment at request time.
// ---------------------------------------------------------------------------

func TestGCSLink_SAReassignment(t *testing.T) {
	f := newGCSFixture(t)
	project := gcsTestProject(t, f, "reassign-project")
	saA := gcsTestSA(t, f.store, project, "reassign-sa-a", "sa-a-reassign@test.iam.gserviceaccount.com")
	saB := gcsTestSA(t, f.store, project, "reassign-sa-b", "sa-b-reassign@test.iam.gserviceaccount.com")
	agent := gcsTestAgent(t, f.store, project, "reassign-agent", gcsAssignedIdentity(saA))
	conv := gcsTestTopic(t, f.store, project, "reassign-topic")
	viewer := gcsTestUser(t, f.store, "reassign-viewer")
	addProjectMemberWithRole(t, f.store, project, viewer.ID, store.GroupMemberRoleMember)
	msg := gcsTestMessage(t, f.store, gcsMessageSpec{
		seed: "reassign-msg", projectID: project.ID, sender: "agent:" + agent.Slug, senderID: agent.ID,
		body: "gs://bkt/o.txt", agentID: agent.ID, conversationID: conv.ID,
	})
	f.source.putObject("bkt", "o.txt", &storage.ObjectAttrs{Size: 2, Generation: 1}, []byte("hi"))

	// Reassign the agent to sa-b.
	agent.AppliedConfig.GCPIdentity = gcsAssignedIdentity(saB)
	require.NoError(t, f.store.UpdateAgent(context.Background(), agent))

	rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "o.txt"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	call, ok := f.gen.lastCall()
	require.True(t, ok)
	require.Equal(t, saB.Email, call.email)

	// Unassign entirely: block mode, no usable SA.
	agent.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{MetadataMode: store.GCPMetadataModeBlock}
	require.NoError(t, f.store.UpdateAgent(context.Background(), agent))

	denyRec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "o.txt"), nil)
	require.Equal(t, http.StatusNotFound, denyRec.Code)
	require.JSONEq(t, gcsExpected404Body, denyRec.Body.String())
}

// TestGCSLink_Deny_CachedEmailStaleAgainstSARecord isolates the SA-email
// match check specifically: ServiceAccountID resolves to a real, current SA
// record (so the lookup itself succeeds), but the agent's own denormalized
// ServiceAccountEmail no longer matches that record's actual email — as it
// would if the SA's email were rotated without updating every agent that
// cached it.
func TestGCSLink_Deny_CachedEmailStaleAgainstSARecord(t *testing.T) {
	f := newGCSFixture(t)
	project := gcsTestProject(t, f, "staleemail-project")
	sa := gcsTestSA(t, f.store, project, "staleemail-sa", "sa-current@test.iam.gserviceaccount.com")
	agent := gcsTestAgent(t, f.store, project, "staleemail-agent", &store.GCPIdentityConfig{
		MetadataMode: store.GCPMetadataModeAssign, ServiceAccountID: sa.ID, ServiceAccountEmail: "sa-stale-cached@test.iam.gserviceaccount.com",
	})
	conv := gcsTestTopic(t, f.store, project, "staleemail-topic")
	msg := gcsTestMessage(t, f.store, gcsMessageSpec{
		seed: "staleemail-msg", projectID: project.ID, sender: "agent:" + agent.Slug, senderID: agent.ID,
		body: "gs://bkt/o.txt", agentID: agent.ID, conversationID: conv.ID,
	})
	viewer := gcsTestUser(t, f.store, "staleemail-viewer")
	addProjectMemberWithRole(t, f.store, project, viewer.ID, store.GroupMemberRoleMember)

	rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "o.txt"), nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.JSONEq(t, gcsExpected404Body, rec.Body.String())
	require.Zero(t, f.gen.mintCount())

	events := f.audit.all()
	require.Len(t, events, 1)
	require.Equal(t, GCSLinkReasonNoSA, events[0].Reason)
}

// ---------------------------------------------------------------------------
// GCS 404 and 403 map to the same 404* body; audit reason differs.
// ---------------------------------------------------------------------------

func TestGCSLink_GCS404And403_SameBodyDifferentAuditReason(t *testing.T) {
	f := newGCSFixture(t)
	viewer, msg := gcsHappyPathFixture(t, f, "gcs404403", "gs://bkt/o.txt")

	f.source.attrsErr = storage.ErrObjectNotExist
	notFoundRec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "o.txt"), nil)
	require.Equal(t, http.StatusNotFound, notFoundRec.Code)

	f.source.attrsErr = &googleapi.Error{Code: http.StatusForbidden}
	forbiddenRec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "o.txt"), nil)
	require.Equal(t, http.StatusNotFound, forbiddenRec.Code)

	require.Equal(t, notFoundRec.Body.String(), forbiddenRec.Body.String())
	require.JSONEq(t, gcsExpected404Body, notFoundRec.Body.String())

	events := f.audit.all()
	require.Len(t, events, 2)
	require.Equal(t, GCSLinkReasonGCSNotFound, events[0].Reason)
	require.Equal(t, GCSLinkReasonGCSDenied, events[1].Reason)
}

// TestGCSLink_AttrsUpstreamError_502 covers the other half of step 11's
// branch: an Attrs error that classifyGCSError cannot map to a specific
// 404/403/401 reason (a GCS 5xx, a timeout, a plain network error) is a 502
// with GCSLinkReasonUpstreamError, never folded into the same 404 body as a
// genuine not-found/denied. Open is never called either way, since the
// handler returns as soon as Attrs fails.
func TestGCSLink_AttrsUpstreamError_502(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "gcs_5xx", err: &googleapi.Error{Code: http.StatusInternalServerError}},
		{name: "plain_network_error", err: errors.New("boom")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGCSFixture(t)
			viewer, msg := gcsHappyPathFixture(t, f, "attrsupstream-"+tc.name, "gs://bkt/o.txt")
			f.source.attrsErr = tc.err

			rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "o.txt"), nil)
			require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())

			_, openCalls := f.source.counts()
			require.Zero(t, openCalls)

			events := f.audit.all()
			require.Len(t, events, 1)
			require.Equal(t, GCSLinkReasonUpstreamError, events[0].Reason)
		})
	}
}

// ---------------------------------------------------------------------------
// Oversized objects: 413 before the reader opens.
// ---------------------------------------------------------------------------

// TestGCSLink_OversizedObject pins the 10 MiB cap as a literal — independent
// of the gcsLinkMaxBytes constant, so a change to the constant's value is
// caught here directly — and then drives both edges of the boundary with
// their own literal sizes: exactly at the cap must be served, one byte over
// must be refused before the reader ever opens.
func TestGCSLink_OversizedObject(t *testing.T) {
	require.EqualValues(t, 10485760, gcsLinkMaxBytes, "the size cap must be exactly 10 MiB")

	t.Run("exactly_at_the_cap_is_served", func(t *testing.T) {
		f := newGCSFixture(t)
		viewer, msg := gcsHappyPathFixture(t, f, "atcap", "gs://bkt/atcap.bin")
		body := bytes.Repeat([]byte("x"), 1024)
		f.source.putObject("bkt", "atcap.bin", &storage.ObjectAttrs{Size: 10485760, Generation: 1}, body)

		rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "atcap.bin"), nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, "10485760", rec.Header().Get("Content-Length"))

		_, openCalls := f.source.counts()
		require.Equal(t, 1, openCalls, "an object exactly at the cap must be read, not refused")
	})

	t.Run("one_byte_over_the_cap_is_refused", func(t *testing.T) {
		f := newGCSFixture(t)
		viewer, msg := gcsHappyPathFixture(t, f, "overcap", "gs://bkt/overcap.bin")
		f.source.putObject("bkt", "overcap.bin", &storage.ObjectAttrs{Size: 10485761, Generation: 1}, nil)

		rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "overcap.bin"), nil)
		require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
		var body ErrorResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		require.Equal(t, gcsErrCodeTooLarge, body.Error.Code)
		require.Equal(t, float64(10485761), body.Error.Details["size"])
		require.Equal(t, float64(10485760), body.Error.Details["limit"])

		_, openCalls := f.source.counts()
		require.Zero(t, openCalls, "the reader must never open for an oversized object")

		events := f.audit.all()
		require.Len(t, events, 1)
		require.Equal(t, GCSLinkReasonTooLarge, events[0].Reason)
	})
}

// TestGCSLink_StreamNeverExceedsAttrsSize isolates step 13's io.LimitReader
// specifically: a GCS response whose actual body is longer than what Attrs
// reported (a misbehaving or lying upstream, not merely stale) must still be
// served as exactly attrs.Size bytes, never more — the size cap enforced at
// step 12 would otherwise be worthless if the stream itself could exceed the
// size it was checked against.
func TestGCSLink_StreamNeverExceedsAttrsSize(t *testing.T) {
	f := newGCSFixture(t)
	viewer, msg := gcsHappyPathFixture(t, f, "overstream", "gs://bkt/o.txt")
	const claimedSize = 5
	actualBody := []byte("hello, this is far more than five bytes")
	require.Greater(t, len(actualBody), claimedSize)
	f.source.putObject("bkt", "o.txt", &storage.ObjectAttrs{Size: claimedSize, Generation: 1}, actualBody)

	rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "o.txt"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, actualBody[:claimedSize], rec.Body.Bytes(), "the response body must be truncated to attrs.Size, never the reader's actual length")
	require.Equal(t, strconv.Itoa(claimedSize), rec.Header().Get("Content-Length"))

	events := f.audit.all()
	require.Len(t, events, 1)
	require.Equal(t, "allow", events[0].Decision)
	require.EqualValues(t, claimedSize, events[0].Bytes, "the audited byte count must match the capped stream, not the reader's actual length")
}

// ---------------------------------------------------------------------------
// Generation pinning: a concurrent overwrite is a 502, never
// different bytes.
// ---------------------------------------------------------------------------

func TestGCSLink_GenerationPinned_ConcurrentOverwrite(t *testing.T) {
	f := newGCSFixture(t)
	viewer, msg := gcsHappyPathFixture(t, f, "genpin", "gs://bkt/o.txt")
	// Attrs reports generation 1 (from the stored object), but the live
	// generation Open() requires has moved to 2 — simulating an overwrite
	// between the two calls.
	f.source.putObject("bkt", "o.txt", &storage.ObjectAttrs{Size: 3, Generation: 1}, []byte("old"))
	overwrittenGeneration := int64(2)
	f.source.mu.Lock()
	f.source.openRequiredGeneration = &overwrittenGeneration
	f.source.mu.Unlock()

	rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "o.txt"), nil)
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	require.NotContains(t, rec.Body.String(), "old")
	require.NotContains(t, rec.Body.String(), "new")

	events := f.audit.all()
	require.Len(t, events, 1)
	require.Equal(t, GCSLinkReasonUpstreamError, events[0].Reason)
}

// ---------------------------------------------------------------------------
// Content typing never trusts stored metadata.
// ---------------------------------------------------------------------------

func TestGCSLink_ContentType_NeverFromMetadata(t *testing.T) {
	f := newGCSFixture(t)

	t.Run("html_metadata_html_body_served_as_text", func(t *testing.T) {
		viewer, msg := gcsHappyPathFixture(t, f, "xsstype", "gs://bkt/evil.html")
		f.source.putObject("bkt", "evil.html", &storage.ObjectAttrs{
			Size: int64(len("<script>alert(1)</script>")), Generation: 1, ContentType: "text/html",
		}, []byte("<script>alert(1)</script>"))

		rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "evil.html"), nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.NotEqual(t, "text/html", rec.Header().Get("Content-Type"))
		require.Equal(t, "text/plain; charset=utf-8", rec.Header().Get("Content-Type"))
		require.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
		require.Contains(t, rec.Header().Get("Content-Disposition"), "attachment")
	})

	t.Run("content_encoding_forces_octet_stream", func(t *testing.T) {
		viewer, msg := gcsHappyPathFixture(t, f, "gziptype", "gs://bkt/data.gz")
		f.source.putObject("bkt", "data.gz", &storage.ObjectAttrs{
			Size: 4, Generation: 1, ContentEncoding: "gzip",
		}, []byte("data"))

		rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "data.gz"), nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, "application/octet-stream", rec.Header().Get("Content-Type"))
		require.Equal(t, "data", rec.Body.String(), "stored bytes must be served as-is, never inflated")
	})

	t.Run("binary_prefix_is_octet_stream", func(t *testing.T) {
		viewer, msg := gcsHappyPathFixture(t, f, "binarytype", "gs://bkt/raw.bin")
		binary := []byte{0x00, 0xFF, 0x00, 0xFF}
		f.source.putObject("bkt", "raw.bin", &storage.ObjectAttrs{Size: int64(len(binary)), Generation: 1}, binary)

		rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "raw.bin"), nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, "application/octet-stream", rec.Header().Get("Content-Type"))
	})

	t.Run("multibyte_rune_split_exactly_at_the_sniff_window_boundary_is_still_text", func(t *testing.T) {
		// 511 ASCII bytes fill the 512-byte sniff window up to its last
		// slot; 'é' (a 2-byte UTF-8 rune, 0xC3 0xA9) starts right there, so
		// its first byte lands inside the window and its second byte lands
		// just outside it — a valid UTF-8 document, split mid-rune purely by
		// where the sniff window happens to end.
		viewer, msg := gcsHappyPathFixture(t, f, "runesplit", "gs://bkt/unicode.txt")
		body := []byte(strings.Repeat("a", 511) + "é tail")
		f.source.putObject("bkt", "unicode.txt", &storage.ObjectAttrs{Size: int64(len(body)), Generation: 1}, body)

		rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "unicode.txt"), nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, "text/plain; charset=utf-8", rec.Header().Get("Content-Type"))
		require.Equal(t, body, rec.Body.Bytes(), "stored bytes must be served as-is regardless of the sniff decision")
	})
}

// gcsPNGBytes, gcsJPEGBytes, gcsGIFBytes and gcsWebPBytes are minimal byte
// strings that http.DetectContentType recognizes as the four supported
// raster image types: each is the type's magic-number prefix followed by NUL
// filler.
func gcsPNGBytes() []byte {
	return append([]byte("\x89PNG\x0D\x0A\x1A\x0A"), bytes.Repeat([]byte{0}, 16)...)
}

func gcsJPEGBytes() []byte {
	return append([]byte("\xFF\xD8\xFF\xE0"), bytes.Repeat([]byte{0}, 16)...)
}

func gcsGIFBytes() []byte {
	return append([]byte("GIF89a"), bytes.Repeat([]byte{0}, 16)...)
}

func gcsWebPBytes() []byte {
	return append([]byte("RIFF\x00\x00\x00\x00WEBPVP8 "), bytes.Repeat([]byte{0}, 16)...)
}

// TestGCSLink_ContentType_ImageSniffing pins the content-typing half of
// image rendering: each of the four supported raster types is recognized
// from its sniffed byte signature alone, and a `.png` object whose actual
// bytes are HTML is never classified as an image, regardless of its
// extension or (per TestGCSLink_ContentType_NeverFromMetadata) its stored
// metadata.
func TestGCSLink_ContentType_ImageSniffing(t *testing.T) {
	f := newGCSFixture(t)

	imageCases := []struct {
		name  string
		body  func() []byte
		ctype string
	}{
		{"png", gcsPNGBytes, "image/png"},
		{"jpeg", gcsJPEGBytes, "image/jpeg"},
		{"gif", gcsGIFBytes, "image/gif"},
		{"webp", gcsWebPBytes, "image/webp"},
	}
	for _, tc := range imageCases {
		t.Run(tc.name+"_sniffed_as_image", func(t *testing.T) {
			seed := "img-" + tc.name
			object := "pic." + tc.name
			viewer, msg := gcsHappyPathFixture(t, f, seed, "gs://bkt/"+object)
			body := tc.body()
			f.source.putObject("bkt", object, &storage.ObjectAttrs{Size: int64(len(body)), Generation: 1}, body)

			rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", object), nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Equal(t, tc.ctype, rec.Header().Get("Content-Type"))
			require.Equal(t, body, rec.Body.Bytes())
		})
	}

	// http.DetectContentType also recognizes BMP and Windows icon/cursor
	// signatures as image types; none of them is on the allow-list. The
	// non-UTF-8 filler keeps the text rule from deciding the result, so
	// each case reaches the image allow-list and must fall through to
	// application/octet-stream. (The standard library has no TIFF or AVIF
	// signature, so neither can reach the allow-list.)
	nonAllowListedImageCases := []struct {
		name          string
		signature     string
		detectedImage string
	}{
		{"bmp", "BM", "image/bmp"},
		{"ico", "\x00\x00\x01\x00", "image/x-icon"},
		{"cur", "\x00\x00\x02\x00", "image/x-icon"},
	}
	for _, tc := range nonAllowListedImageCases {
		t.Run(tc.name+"_signature_is_not_an_image", func(t *testing.T) {
			body := append([]byte(tc.signature), bytes.Repeat([]byte{0xFF}, 16)...)
			require.Equal(t, tc.detectedImage, http.DetectContentType(body),
				"the vector must be one the standard sniffer classifies as an image")

			object := "pic." + tc.name
			viewer, msg := gcsHappyPathFixture(t, f, "img-"+tc.name, "gs://bkt/"+object)
			f.source.putObject("bkt", object, &storage.ObjectAttrs{Size: int64(len(body)), Generation: 1}, body)

			rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", object), nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			ct := rec.Header().Get("Content-Type")
			require.False(t, strings.HasPrefix(ct, "image/"), "content type %q must not be an image type", ct)
			require.Equal(t, "application/octet-stream", ct)
		})
	}

	t.Run("png_extension_with_html_bytes_is_not_an_image", func(t *testing.T) {
		viewer, msg := gcsHappyPathFixture(t, f, "img-fake-png", "gs://bkt/fake.png")
		body := []byte("<html><body>not a png</body></html>")
		f.source.putObject("bkt", "fake.png", &storage.ObjectAttrs{Size: int64(len(body)), Generation: 1}, body)

		rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "fake.png"), nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		ct := rec.Header().Get("Content-Type")
		require.NotContains(t, ct, "image/", "a .png object whose bytes are HTML must not be served as an image")
		require.NotEqual(t, "text/html", ct)
		require.Equal(t, "text/plain; charset=utf-8", ct)
	})

	t.Run("svg_bytes_are_never_an_image_or_svg_xml", func(t *testing.T) {
		viewer, msg := gcsHappyPathFixture(t, f, "img-svg", "gs://bkt/pic.svg")
		body := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><circle r="1"/></svg>`)
		f.source.putObject("bkt", "pic.svg", &storage.ObjectAttrs{Size: int64(len(body)), Generation: 1}, body)

		rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "pic.svg"), nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		ct := rec.Header().Get("Content-Type")
		require.NotContains(t, ct, "image/", "an svg object must never be served as an image type")
		require.NotEqual(t, "image/svg+xml", ct)
		require.Equal(t, "text/plain; charset=utf-8", ct)
	})

	t.Run("xml_declared_svg_bytes_are_never_an_image_or_svg_xml", func(t *testing.T) {
		// Unlike a bare <svg ...> root, this prefix matches
		// http.DetectContentType's own "<?xml" signature (text/xml), a
		// different non-image, non-svg+xml result than the bare case above -
		// exercised separately so a future image-detection change cannot
		// special-case one SVG spelling and miss the other.
		viewer, msg := gcsHappyPathFixture(t, f, "img-svg-xmldecl", "gs://bkt/pic2.svg")
		body := []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"></svg>`)
		f.source.putObject("bkt", "pic2.svg", &storage.ObjectAttrs{Size: int64(len(body)), Generation: 1}, body)

		rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "pic2.svg"), nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		ct := rec.Header().Get("Content-Type")
		require.NotContains(t, ct, "image/")
		require.NotEqual(t, "image/svg+xml", ct)
	})

	t.Run("content_encoding_overrides_an_otherwise_valid_image_signature", func(t *testing.T) {
		// A real gzip-encoded object's stored bytes are compressed and would
		// not actually carry a raw PNG signature, but the ordering this
		// pins - Content-Encoding is checked before any image sniff - must
		// hold regardless of what the sniffed bytes happen to look like.
		body := gcsPNGBytes()
		viewer, msg := gcsHappyPathFixture(t, f, "img-gzip-png", "gs://bkt/weird.png.gz")
		f.source.putObject("bkt", "weird.png.gz", &storage.ObjectAttrs{
			Size: int64(len(body)), Generation: 1, ContentEncoding: "gzip",
		}, body)

		rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "weird.png.gz"), nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, "application/octet-stream", rec.Header().Get("Content-Type"))
	})
}

// ---------------------------------------------------------------------------
// Exactly one audit event per request, with the listed fields.
// ---------------------------------------------------------------------------

func TestGCSLink_AuditEvent_Allow(t *testing.T) {
	f := newGCSFixture(t)
	viewer, msg := gcsHappyPathFixture(t, f, "audit-allow", "gs://bkt/o.txt")
	f.source.putObject("bkt", "o.txt", &storage.ObjectAttrs{Size: 2, Generation: 7}, []byte("hi"))

	rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "o.txt"), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	events := f.audit.all()
	require.Len(t, events, 1)
	e := events[0]
	require.Equal(t, "allow", e.Decision)
	require.Equal(t, GCSLinkReasonOK, e.Reason)
	require.Equal(t, http.StatusOK, e.Status)
	require.Equal(t, viewer.ID, e.ViewerUserID)
	require.Equal(t, msg.ID, e.MessageID)
	require.Equal(t, msg.ConversationID, e.ConversationID)
	require.Equal(t, tid("audit-allow-agent"), e.SenderAgentID)
	require.Equal(t, "sa-a-audit-allow@test.iam.gserviceaccount.com", e.SAEmail)
	require.Equal(t, "bkt", e.Bucket)
	require.Equal(t, "o.txt", e.Object)
	require.Equal(t, int64(2), e.Bytes)
	require.Equal(t, int64(7), e.Generation)
	require.GreaterOrEqual(t, e.DurationMS, int64(0))
}

func TestGCSLink_AuditEvent_Deny(t *testing.T) {
	f := newGCSFixture(t)
	viewer, _ := gcsHappyPathFixture(t, f, "audit-deny", "gs://bkt/o.txt")

	rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(tid("audit-deny-nope"), "bkt", "o.txt"), nil)
	require.Equal(t, http.StatusNotFound, rec.Code)

	events := f.audit.all()
	require.Len(t, events, 1)
	require.Equal(t, "deny", events[0].Decision)
	require.Equal(t, GCSLinkReasonMessageNotFound, events[0].Reason)
	require.Equal(t, http.StatusNotFound, events[0].Status)
	require.Equal(t, viewer.ID, events[0].ViewerUserID)
}

// TestGCSLink_AuditEvent_StreamFailurePartway covers a stream that fails
// after headers (and some real bytes) are already on the wire: the response
// status can no longer change, but the audit record must not claim a clean
// full-object allow. attrs.Size (700) is deliberately larger than the fake
// source's actual body (600 bytes, itself past the 512-byte sniff window),
// so io.Copy keeps reading past the real content and hits the fake's
// substituted stream error instead of a clean EOF.
func TestGCSLink_AuditEvent_StreamFailurePartway(t *testing.T) {
	f := newGCSFixture(t)
	viewer, msg := gcsHappyPathFixture(t, f, "streamfail", "gs://bkt/o.txt")
	body := bytes.Repeat([]byte("x"), 600)
	f.source.putObject("bkt", "o.txt", &storage.ObjectAttrs{Size: 700, Generation: 1}, body)
	f.source.mu.Lock()
	f.source.openStreamFailErr = errors.New("simulated mid-stream network failure")
	f.source.mu.Unlock()

	rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "o.txt"), nil)
	// The status is already 200 on the wire by the time the stream fails —
	// there is no way to turn it into an error response at this point.
	require.Equal(t, http.StatusOK, rec.Code)

	events := f.audit.all()
	require.Len(t, events, 1)
	require.Equal(t, "allow", events[0].Decision)
	require.Equal(t, GCSLinkReasonUpstreamError, events[0].Reason)
	require.Equal(t, http.StatusOK, events[0].Status)
	require.Less(t, events[0].Bytes, int64(700), "a partial stream must not be audited as the full object size")
}

// ---------------------------------------------------------------------------
// Rate limit (61st/min -> 429) and concurrency (17th -> 429).
// ---------------------------------------------------------------------------

func TestGCSLink_RateLimit_61stRequestPerMinute(t *testing.T) {
	f := newGCSFixture(t)
	viewer, msg := gcsHappyPathFixture(t, f, "ratelimit", "gs://bkt/o.txt")
	f.source.putObject("bkt", "o.txt", &storage.ObjectAttrs{Size: 2, Generation: 1}, []byte("hi"))

	// Near-zero refill: the 61st request is rejected no matter how long the
	// first 60 take to run, so the assertion below does not depend on wall
	// clock scheduling (the production limiter is wired with a real refill
	// rate; that wiring is asserted separately below).
	f.srv.gcsLinkRateLimiter = NewGCPTokenRateLimiter(1e-9, gcsLinkRateLimitPerMinute)

	for i := 0; i < gcsLinkRateLimitPerMinute; i++ {
		rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "o.txt"), nil)
		require.Equal(t, http.StatusOK, rec.Code, "request %d should be allowed: %s", i+1, rec.Body.String())
	}
	before := len(f.audit.all())
	rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "o.txt"), nil)
	require.Equal(t, http.StatusTooManyRequests, rec.Code, "the 61st request in the same minute must be rate limited")

	events := f.audit.all()
	require.Len(t, events, before+1)
	require.Equal(t, GCSLinkReasonRateLimited, events[before].Reason)
}

// TestGCSLink_LimiterProductionWiring pins the literal production limits —
// 60 requests per minute per user (1/s refill, burst 60) and 16 concurrent
// fetches — independent of the deterministic near-zero-refill limiter
// TestGCSLink_RateLimit_61stRequestPerMinute substitutes to make its own
// assertion immune to scheduling delays.
func TestGCSLink_LimiterProductionWiring(t *testing.T) {
	f := newGCSFixture(t)
	require.Equal(t, 1.0, f.srv.gcsLinkRateLimiter.rate)
	require.Equal(t, 60, f.srv.gcsLinkRateLimiter.burst)
	require.Equal(t, 16, cap(f.srv.gcsLinkSem))
}

func TestGCSLink_ConcurrencyLimit_17thConcurrentRequest(t *testing.T) {
	f := newGCSFixture(t)
	viewer, msg := gcsHappyPathFixture(t, f, "concurrency", "gs://bkt/o.txt")
	f.source.putObject("bkt", "o.txt", &storage.ObjectAttrs{Size: 2, Generation: 1}, []byte("hi"))

	block := make(chan struct{})
	f.source.mu.Lock()
	f.source.attrsBlock = block
	f.source.mu.Unlock()

	const n = gcsLinkGlobalConcurrency
	results := make(chan int, n)
	for i := 0; i < n; i++ {
		go func() {
			rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "o.txt"), nil)
			results <- rec.Code
		}()
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		attrsCalls, _ := f.source.counts()
		if attrsCalls >= n {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d concurrent Attrs calls, saw %d", n, attrsCalls)
		}
		time.Sleep(5 * time.Millisecond)
	}

	overflowRec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "o.txt"), nil)
	require.Equal(t, http.StatusTooManyRequests, overflowRec.Code, "the 17th concurrent request must be rejected")

	close(block)
	for i := 0; i < n; i++ {
		code := <-results
		require.Equal(t, http.StatusOK, code, "a blocked request should complete once the slot frees up")
	}
}

// ---------------------------------------------------------------------------
// Enumeration: byte-identical 404 body AND headers across every deny path.
// ---------------------------------------------------------------------------

func TestGCSLink_Enumeration_ByteIdenticalAcrossDenyPaths(t *testing.T) {
	f := newGCSFixture(t)
	viewer, msg := gcsHappyPathFixture(t, f, "enum", "gs://bkt/o.txt")
	f.source.putObject("bkt", "o.txt", &storage.ObjectAttrs{Size: 2, Generation: 1}, []byte("hi"))

	unknownMsg := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(tid("enum-unknown"), "bkt", "o.txt"), nil)

	f.source.attrsErr = storage.ErrObjectNotExist
	gcsNotFound := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "o.txt"), nil)
	f.source.attrsErr = &googleapi.Error{Code: http.StatusForbidden}
	gcsDenied := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "o.txt"), nil)
	f.source.attrsErr = nil

	outsider := gcsTestUser(t, f.store, "enum-outsider")
	unreadable := doRequestAsUser(t, f.srv, outsider, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "o.txt"), nil)

	recorders := []*httptest.ResponseRecorder{unknownMsg, gcsNotFound, gcsDenied, unreadable}
	for i, r := range recorders {
		require.Equal(t, http.StatusNotFound, r.Code, "recorder %d", i)
		require.JSONEq(t, gcsExpected404Body, r.Body.String(), "recorder %d body", i)
	}
	base := recorders[0]
	for i, r := range recorders[1:] {
		require.Equal(t, base.Body.String(), r.Body.String(), "recorder %d body differs from base", i+1)
		require.Equal(t, base.Header().Get("Content-Type"), r.Header().Get("Content-Type"), "recorder %d Content-Type differs", i+1)
		require.Empty(t, r.Header().Get("Content-Disposition"), "a deny response must never carry a disposition header")
		require.Empty(t, r.Header().Get("X-Scion-Gcs-Generation"), "a deny response must never carry a generation header")
	}
}

// ---------------------------------------------------------------------------
// Method not allowed.
// ---------------------------------------------------------------------------

func TestGCSLink_MethodNotAllowed(t *testing.T) {
	f := newGCSFixture(t)
	viewer, msg := gcsHappyPathFixture(t, f, "method", "gs://bkt/o.txt")
	rec := doRequestAsUser(t, f.srv, viewer, http.MethodPost, gcsRequestPath(msg.ID, "bkt", "o.txt"), nil)
	require.Equal(t, http.StatusMethodNotAllowed, rec.Code)

	// Exactly one audit event is emitted per request, including a
	// wrong-method call — it has no reason enum value of its own, so it is
	// recorded as bad_request.
	events := f.audit.all()
	require.Len(t, events, 1)
	require.Equal(t, "deny", events[0].Decision)
	require.Equal(t, GCSLinkReasonBadRequest, events[0].Reason)
	require.Equal(t, http.StatusMethodNotAllowed, events[0].Status)
}

// ---------------------------------------------------------------------------
// Unit-level coverage for the standalone helpers.
// ---------------------------------------------------------------------------

func TestGCSLink_ValidateGCSObjectParams(t *testing.T) {
	validUUID := tid("valid-message")
	cases := []struct {
		name                      string
		messageID, bucket, object string
		want                      bool
	}{
		{"valid", validUUID, "bkt", "o.txt", true},
		{"invalid-message-id", "not-a-uuid", "bkt", "o.txt", false},
		{"bucket-too-short", validUUID, "ab", "o.txt", false},
		{"bucket-uppercase", validUUID, "BKT", "o.txt", false},
		{"bucket-double-dot", validUUID, "bk..t", "o.txt", false},
		{"bucket-leading-dash", validUUID, "-bkt", "o.txt", false},
		{"object-trailing-dot", validUUID, "bkt", "o.txt.", false},
		{"object-too-long", validUUID, "bkt", strings.Repeat("a", 1025), false},
		{"object-space", validUUID, "bkt", "o file.txt", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := validateGCSObjectParams(c.messageID, c.bucket, c.object)
			require.Equal(t, c.want, got)
		})
	}
}

func TestGCSLink_ParseGCSObjectQuery(t *testing.T) {
	mk := func(raw string) *http.Request {
		req, err := http.NewRequest(http.MethodGet, "/api/v1/gcs/object?"+raw, nil)
		require.NoError(t, err)
		return req
	}
	mid := tid("parse-query-msg")

	_, _, _, ok := parseGCSObjectQuery(mk("message=" + mid + "&bucket=b&object=o"))
	require.True(t, ok)

	_, _, _, ok = parseGCSObjectQuery(mk("message=" + mid + "&bucket=b&object=o&agent=x"))
	require.False(t, ok, "an extra key must be rejected")

	_, _, _, ok = parseGCSObjectQuery(mk("message=" + mid + "&bucket=b"))
	require.False(t, ok, "a missing key must be rejected")

	_, _, _, ok = parseGCSObjectQuery(mk("message=" + mid + "&bucket=b&object=o&object=p"))
	require.False(t, ok, "a duplicated key must be rejected")
}

func TestGCSLink_ClassifyGCSError(t *testing.T) {
	require.Equal(t, GCSLinkReasonGCSNotFound, classifyGCSError(storage.ErrObjectNotExist))
	require.Equal(t, GCSLinkReasonGCSNotFound, classifyGCSError(&googleapi.Error{Code: http.StatusNotFound}))
	require.Equal(t, GCSLinkReasonGCSDenied, classifyGCSError(&googleapi.Error{Code: http.StatusForbidden}))
	require.Equal(t, GCSLinkReasonGCSDenied, classifyGCSError(&googleapi.Error{Code: http.StatusUnauthorized}))
	require.Equal(t, GCSLinkReasonUpstreamError, classifyGCSError(&googleapi.Error{Code: http.StatusPreconditionFailed}))
	require.Equal(t, GCSLinkReasonUpstreamError, classifyGCSError(errors.New("boom")))
}

func TestGCSLink_ContentDisposition_RFC5987Encoding(t *testing.T) {
	require.Equal(t, `attachment; filename*=UTF-8''o.txt`, gcsContentDisposition("o.txt"))
	require.Equal(t, `attachment; filename*=UTF-8''a%20b.txt`, gcsContentDisposition("a b.txt"))
	require.Equal(t, `attachment; filename*=UTF-8''a%22b.txt`, gcsContentDisposition(`a"b.txt`))
	require.Equal(t, `attachment; filename*=UTF-8''a%2Cb%40c.txt`, gcsContentDisposition("a,b@c.txt"))
}

func TestGCSLink_ObjectBasename(t *testing.T) {
	require.Equal(t, "o.txt", gcsObjectBasename("o.txt"))
	require.Equal(t, "o.txt", gcsObjectBasename("dir/sub/o.txt"))
	require.Equal(t, "", gcsObjectBasename("dir/"))
}

// TestGCSLink_Deny_ConversationLookupFails covers a message whose
// ConversationID is well-formed but names no real conversation (as opposed
// to TestGCSLink_Deny_MessageIDUnknown, which never reaches
// canUserReadMessage at all). This is what actually isolates
// canUserReadMessage's GetConversation-error check: msg.ConversationID == ""
// is denied by that same check already (an empty id fails store parsing
// exactly like a well-formed-but-missing one), so this is the only test that
// can tell the two apart from a crash — without the check, the code would
// dereference a nil *store.Conversation.
func TestGCSLink_Deny_ConversationLookupFails(t *testing.T) {
	f := newGCSFixture(t)
	project := gcsTestProject(t, f, "noconv-project")
	sa := gcsTestSA(t, f.store, project, "noconv-sa", "sa-noconv@test.iam.gserviceaccount.com")
	agent := gcsTestAgent(t, f.store, project, "noconv-agent", gcsAssignedIdentity(sa))
	viewer := gcsTestUser(t, f.store, "noconv-viewer")
	addProjectMemberWithRole(t, f.store, project, viewer.ID, store.GroupMemberRoleMember)
	msg := gcsTestMessage(t, f.store, gcsMessageSpec{
		seed: "noconv-msg", projectID: project.ID, sender: "agent:" + agent.Slug, senderID: agent.ID,
		body: "gs://bkt/o.txt", agentID: agent.ID, conversationID: tid("noconv-nonexistent-conversation"),
	})

	rec := doRequestAsUser(t, f.srv, viewer, http.MethodGet, gcsRequestPath(msg.ID, "bkt", "o.txt"), nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.JSONEq(t, gcsExpected404Body, rec.Body.String())
	require.Zero(t, f.gen.mintCount())
}

// gcsFailingProjectStore wraps a store.Store, injecting a non-not-found
// error from GetProject, so a test can prove canReadGroupConversation
// propagates a genuine store fault distinctly from an ordinary "not found"
// decision.
type gcsFailingProjectStore struct {
	store.Store
	err error
}

func (f *gcsFailingProjectStore) GetProject(ctx context.Context, id string) (*store.Project, error) {
	return nil, f.err
}

// gcsTestIdentity is a minimal Identity for direct (non-HTTP) unit tests of
// canReadGroupConversation.
type gcsTestIdentity struct {
	id, kind string
}

func (i gcsTestIdentity) ID() string   { return i.id }
func (i gcsTestIdentity) Type() string { return i.kind }

// TestGCSLink_CanReadGroupConversation directly unit-tests the extracted helper,
// including the GetProject error-vs-not-found distinction that no caller
// today observably depends on (canUserReadMessage collapses both to a
// single false), but which the function's own contract still commits to.
func TestGCSLink_CanReadGroupConversation(t *testing.T) {
	ctx := context.Background()

	t.Run("nil identity denies", func(t *testing.T) {
		srv, s := testServer(t)
		conv := &store.Conversation{ID: tid("crg-nilident"), Kind: "group", Surface: "native", DriftState: "active", LastActivityAt: time.Now(), CreatedAt: time.Now()}
		require.NoError(t, s.CreateConversation(ctx, conv))
		allowed, err := srv.canReadGroupConversation(ctx, nil, conv)
		require.NoError(t, err)
		require.False(t, allowed)
	})

	t.Run("projectless legacy group: participant allowed, non-participant denied", func(t *testing.T) {
		srv, s := testServer(t)
		conv := &store.Conversation{ID: tid("crg-legacy"), Kind: "group", Surface: "native", DriftState: "active", LastActivityAt: time.Now(), CreatedAt: time.Now()}
		require.NoError(t, s.CreateConversation(ctx, conv))
		member := gcsTestUser(t, s, "crg-legacy-member")
		outsider := gcsTestUser(t, s, "crg-legacy-outsider")
		require.NoError(t, s.AddParticipant(ctx, &store.ConversationParticipant{
			ID: tid("crg-legacy-part"), ConversationID: conv.ID, PrincipalKind: "user", PrincipalID: member.ID,
			Role: "member", JoinedAt: time.Now(),
		}))

		allowed, err := srv.canReadGroupConversation(ctx, gcsTestIdentity{id: member.ID, kind: "user"}, conv)
		require.NoError(t, err)
		require.True(t, allowed)

		allowed, err = srv.canReadGroupConversation(ctx, gcsTestIdentity{id: outsider.ID, kind: "user"}, conv)
		require.NoError(t, err)
		require.False(t, allowed)
	})

	t.Run("project-scoped group: an agent from a different project is denied even with a role binding on this one", func(t *testing.T) {
		srv, s := testServer(t)
		fixture := &gcsFixture{srv: srv, store: s}
		project := gcsTestProject(t, fixture, "crg-agentproject")
		homeProject := gcsTestProject(t, fixture, "crg-agent-home-project")
		conv := &store.Conversation{
			ID: tid("crg-agentconv"), ProjectID: &project.ID, Kind: "group", Surface: "native",
			DriftState: "active", LastActivityAt: time.Now(), CreatedAt: time.Now(),
		}
		require.NoError(t, s.CreateConversation(ctx, conv))
		foreignAgentRecord := gcsTestAgent(t, s, homeProject, "crg-foreign-agent", nil)
		// Grant the agent a real role binding on the *conv's* project, so
		// removing the cross-project short-circuit would let CheckAccess
		// independently allow it — proving this check, not merely
		// CheckAccess's own default deny, is what stops the agent.
		grantAgentProjectAccess(t, s, foreignAgentRecord.ID, project.ID)

		foreignAgent := &agentIdentityWrapper{&AgentTokenClaims{
			Claims:    jwt.Claims{Subject: foreignAgentRecord.ID},
			ProjectID: homeProject.ID,
			Scopes:    []AgentTokenScope{ScopeProjectRead},
		}}
		allowed, err := srv.canReadGroupConversation(ctx, foreignAgent, conv)
		require.NoError(t, err)
		require.False(t, allowed)
	})

	t.Run("project-scoped group: GetProject not-found denies with no error", func(t *testing.T) {
		srv, s := testServer(t)
		missingProjectID := tid("crg-missing-project")
		conv := &store.Conversation{
			ID: tid("crg-noproject"), ProjectID: &missingProjectID, Kind: "group", Surface: "native",
			DriftState: "active", LastActivityAt: time.Now(), CreatedAt: time.Now(),
		}
		require.NoError(t, s.CreateConversation(ctx, conv))

		allowed, err := srv.canReadGroupConversation(ctx, gcsTestIdentity{id: tid("crg-someone"), kind: "user"}, conv)
		require.NoError(t, err)
		require.False(t, allowed)
	})

	t.Run("project-scoped group: a genuine store fault propagates as an error, not a silent deny", func(t *testing.T) {
		srv, s := testServer(t)
		project := gcsTestProject(t, &gcsFixture{srv: srv, store: s}, "crg-faultproject")
		conv := &store.Conversation{
			ID: tid("crg-fault"), ProjectID: &project.ID, Kind: "group", Surface: "native",
			DriftState: "active", LastActivityAt: time.Now(), CreatedAt: time.Now(),
		}
		require.NoError(t, s.CreateConversation(ctx, conv))

		boom := errors.New("boom: database is on fire")
		srv.store = &gcsFailingProjectStore{Store: s, err: boom}

		allowed, err := srv.canReadGroupConversation(ctx, gcsTestIdentity{id: tid("crg-someone-2"), kind: "user"}, conv)
		require.ErrorIs(t, err, boom)
		require.False(t, allowed)
	})
}

// ---------------------------------------------------------------------------
// newGCSLinkBaseTransport must not panic if http.DefaultTransport has been
// replaced or wrapped by something other than *http.Transport (e.g. an
// otel/tracing instrumentation shim).
// ---------------------------------------------------------------------------

// fakeNonTransportRoundTripper is a http.RoundTripper that is deliberately
// not a *http.Transport, standing in for an instrumentation wrapper that
// replaces the package-level http.DefaultTransport var.
type fakeNonTransportRoundTripper struct{}

func (fakeNonTransportRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("fakeNonTransportRoundTripper: not implemented")
}

// TestGCSLink_NewBaseTransport_SurvivesReplacedDefaultTransport is not
// t.Parallel: it mutates the package-level http.DefaultTransport var for its
// duration and restores it via t.Cleanup.
func TestGCSLink_NewBaseTransport_SurvivesReplacedDefaultTransport(t *testing.T) {
	original := http.DefaultTransport
	http.DefaultTransport = fakeNonTransportRoundTripper{}
	t.Cleanup(func() { http.DefaultTransport = original })

	transport := newGCSLinkBaseTransport()
	require.NotNil(t, transport)
	require.NotEqual(t, http.RoundTripper(fakeNonTransportRoundTripper{}), http.RoundTripper(transport))

	// The fallback must reproduce Go's own http.DefaultTransport settings.
	require.NotNil(t, transport.Proxy)
	require.True(t, transport.ForceAttemptHTTP2)
	require.Equal(t, 100, transport.MaxIdleConns)
	require.Equal(t, 90*time.Second, transport.IdleConnTimeout)
	require.Equal(t, 10*time.Second, transport.TLSHandshakeTimeout)
	require.Equal(t, time.Second, transport.ExpectContinueTimeout)
}

// TestGCSLink_NewBaseTransport_ClonesRealDefaultTransport covers the
// ordinary case (http.DefaultTransport is still a *http.Transport, as in
// production) side by side with the fallback case above, so a future change
// cannot satisfy one path while silently breaking the other.
func TestGCSLink_NewBaseTransport_ClonesRealDefaultTransport(t *testing.T) {
	require.IsType(t, &http.Transport{}, http.DefaultTransport)
	transport := newGCSLinkBaseTransport()
	require.NotNil(t, transport)
	require.NotSame(t, http.DefaultTransport.(*http.Transport), transport, "must be a Clone(), never the shared DefaultTransport itself")
}

// TestGCSLink_RequestDeadlineProductionValue pins the whole-request deadline
// as a literal, and checks that a server with no test override uses it.
func TestGCSLink_RequestDeadlineProductionValue(t *testing.T) {
	require.Equal(t, 60*time.Second, gcsLinkRequestDeadline, "the request deadline must be exactly 60s")
	f := newGCSFixture(t)
	require.Equal(t, 60*time.Second, gcsLinkEffectiveRequestDeadline(f.srv))
}
