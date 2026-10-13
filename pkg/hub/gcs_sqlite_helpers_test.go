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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/googleapi"
)

// fakeGCSJSONServer is a minimal subset of the GCS JSON API: an object
// metadata GET and a media (alt=media) GET, with ifGenerationMatch
// precondition support. It records every request's Authorization header and
// query string so the test can assert on them: this test runs the
// production source against an httptest.Server that speaks a minimal GCS
// JSON API subset, asserting the Authorization: Bearer <minted> header,
// generation pinning, 404/403 mapping and ReadCompressed.
type fakeGCSJSONServer struct {
	mu       sync.Mutex
	requests []*http.Request

	name            string
	size            int64
	generation      int64
	contentType     string
	contentEncoding string
	body            []byte

	// denyMetadata/denyMedia, when non-zero, make the corresponding call
	// respond with that HTTP status instead of succeeding.
	denyMetadataStatus int
	denyMediaStatus    int

	// metadataBlock/mediaBlock, when non-nil, make the corresponding call
	// wait to respond until the channel is closed, simulating a hung
	// upstream GCS call.
	metadataBlock chan struct{}
	mediaBlock    chan struct{}
}

// fakeGCSTokenGenerator is a GCPTokenGenerator that records every mint call
// instead of talking to IAM. Handler-level tests substitute this so a
// zero-mint assertion is a plain call-count check.
type fakeGCSTokenGenerator struct {
	mu       sync.Mutex
	calls    []fakeGCSMintCall
	failMint bool
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
	return gcsFixtureFor(t, srv, s)
}

// gcsFixtureFor wires the gs:// link fakes onto an already built server; s
// is the raw store setup writes through.
func gcsFixtureFor(t *testing.T, srv *Server, s store.Store) *gcsFixture {
	t.Helper()
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

// gcsTestSA creates a verified service account scoped to project: the
// state an agent's assignment must be in for the hub to mint its token, and
// so for a gs:// link to open.
func gcsTestSA(t *testing.T, s store.Store, project *store.Project, seed, email string) *store.GCPServiceAccount {
	t.Helper()
	return gcsTestSAWith(t, s, project, seed, email, nil)
}

// gcsTestSAWith is gcsTestSA with a hook that adjusts the account before it
// is stored (for example to leave it unverified or make it hub-scoped).
func gcsTestSAWith(t *testing.T, s store.Store, project *store.Project, seed, email string, adjust func(*store.GCPServiceAccount)) *store.GCPServiceAccount {
	t.Helper()
	sa := &store.GCPServiceAccount{
		ID: tid(seed), Scope: store.ScopeProject, ScopeID: project.ID, Email: email,
		ProjectID: project.ID, CreatedAt: time.Now(),
		Verified: true, VerificationStatus: store.GCPVerificationVerified,
	}
	if adjust != nil {
		adjust(sa)
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

const gcsExpected404Body = `{"error":{"code":"not_found","message":"Object not found"}}`

func (f *fakeGCSJSONServer) lastAuthHeader() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return ""
	}
	return f.requests[len(f.requests)-1].Header.Get("Authorization")
}

// ServeHTTP routes two request shapes, matching the real storage client's
// own split: Attrs always uses the JSON metadata API
// (.../b/{bucket}/o/{object}, a single percent-encoded object segment), and
// NewReader — since production code never sets storage.WithJSONReads — uses
// the plain "XML" media GET (/{bucket}/{object}, literal path segments, no
// query string), authenticated by the same underlying http.Client either
// way. The precondition on the media path arrives as the
// x-goog-if-generation-match header, not a query parameter.
func (f *fakeGCSJSONServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Clone(context.Background()))
	f.mu.Unlock()

	writeGoogleAPIError := func(status int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, `{"error":{"code":%d,"message":"denied"}}`, status)
	}

	if idx := strings.Index(r.URL.Path, "/b/"); idx != -1 {
		rest := r.URL.Path[idx+len("/b/"):]
		if parts := strings.SplitN(rest, "/o/", 2); len(parts) == 2 {
			// Metadata GET (alt=json).
			if f.metadataBlock != nil {
				<-f.metadataBlock
			}
			if f.denyMetadataStatus != 0 {
				writeGoogleAPIError(f.denyMetadataStatus)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{"name":%q,"size":"%d","generation":"%d","contentType":%q,"contentEncoding":%q}`,
				f.name, f.size, f.generation, f.contentType, f.contentEncoding)
			return
		}
	}

	// Media GET (XML-style): /{bucket}/{object}.
	if f.mediaBlock != nil {
		<-f.mediaBlock
	}
	if f.denyMediaStatus != 0 {
		writeGoogleAPIError(f.denyMediaStatus)
		return
	}
	if ig := r.Header.Get("x-goog-if-generation-match"); ig != "" {
		want, _ := strconv.ParseInt(ig, 10, 64)
		if want != f.generation {
			writeGoogleAPIError(http.StatusPreconditionFailed)
			return
		}
	}
	// A real Content-Encoding header is what makes ReadCompressed(true) mean
	// anything to Go's http.Transport: without it, there is nothing to
	// transparently decompress either way, and the flag makes no observable
	// difference (see TestGCSLink_RealClient_ReadCompressedServesStoredBytes).
	if f.contentEncoding != "" {
		w.Header().Set("Content-Encoding", f.contentEncoding)
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(f.body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(f.body)
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

// fakeGCSMintCall records one GenerateAccessToken call.
type fakeGCSMintCall struct {
	email  string
	scopes []string
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

func newFakeGCSSource() *fakeGCSSource {
	return &fakeGCSSource{objects: make(map[string]fakeGCSStoredObject)}
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

func newGCSAuditRecorder() *gcsAuditRecorder {
	return &gcsAuditRecorder{LogAuditLogger: NewLogAuditLogger("[test]", false)}
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

func gcsFakeObjectKey(bucket, object string) string { return bucket + "/" + object }

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
