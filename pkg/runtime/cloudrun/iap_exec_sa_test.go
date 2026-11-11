package cloudrun

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"cloud.google.com/go/auth"
	"cloud.google.com/go/run/apiv2/runpb"
	"github.com/googleapis/gax-go/v2"
	"golang.org/x/crypto/ssh"
)

const (
	testProject  = "my-project"
	testLocation = "us-central1"
)

func testInstanceName(shortID string) string {
	return "projects/" + testProject + "/locations/" + testLocation + "/instances/" + shortID
}

// fakeInstanceGetter implements InstancesAPI's GetInstance and Close; the
// remaining methods are not used by the SSH key signing path. Instances are
// looked up by resource name.
type fakeInstanceGetter struct {
	InstancesAPI
	mu      sync.Mutex
	insts   map[string]*runpb.Instance
	err     error
	getReqs []*runpb.GetInstanceRequest
	// onGet, when set, runs at the start of every GetInstance.
	onGet      func()
	closeCount int
}

func (f *fakeInstanceGetter) GetInstance(_ context.Context, req *runpb.GetInstanceRequest, _ ...gax.CallOption) (*runpb.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getReqs = append(f.getReqs, req)
	if f.onGet != nil {
		f.onGet()
	}
	if f.err != nil {
		return nil, f.err
	}
	return f.insts[req.GetName()], nil
}

func (f *fakeInstanceGetter) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeCount++
	return nil
}

// testClock is a settable clock for signed key expiry, safe for concurrent
// use.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newTestClock() *testClock { return &testClock{t: time.Unix(1_800_000_000, 0)} }

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func (c *testClock) validBefore(d time.Duration) uint64 { return uint64(c.now().Add(d).Unix()) }

// newTestConnector returns a connector whose Instances API client is
// newClient, whose project number is always 123456 and whose clock is clock.
func newTestConnector(newClient func(context.Context) (InstancesAPI, error), clock *testClock) *IAPExecConnector {
	return &IAPExecConnector{
		newInstancesClient:  newClient,
		lookupProjectNumber: func(context.Context, string) (string, error) { return "123456", nil },
		now:                 clock.now,
	}
}

func fakeClient(f *fakeInstanceGetter) func(context.Context) (InstancesAPI, error) {
	return func(context.Context) (InstancesAPI, error) { return f, nil }
}

// osLoginServer records signSshPublicKey request bodies. When certValidity is
// set it answers with a real OpenSSH certificate valid for that long from the
// clock's current time; otherwise with an unparseable placeholder.
type osLoginServer struct {
	mu     sync.Mutex
	bodies []map[string]string
}

// count returns how many sign requests were received.
func (s *osLoginServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bodies)
}

// body returns the i-th sign request body.
func (s *osLoginServer) body(i int) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bodies[i]
}

func startOSLoginServer(t *testing.T, clock *testClock, certValidity time.Duration) *osLoginServer {
	t.Helper()
	_, caKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	ca, err := ssh.NewSignerFromKey(caKey)
	if err != nil {
		t.Fatalf("CA signer: %v", err)
	}
	s := &osLoginServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode sign request body: %v", err)
		}
		s.mu.Lock()
		s.bodies = append(s.bodies, body)
		n := len(s.bodies)
		s.mu.Unlock()
		signed := fmt.Sprintf("signed-cert-%d", n)
		if certValidity > 0 {
			pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(body["sshPublicKey"]))
			if err != nil {
				t.Errorf("parse sshPublicKey: %v", err)
			}
			cert := &ssh.Certificate{Key: pub, CertType: ssh.UserCert, ValidBefore: clock.validBefore(certValidity)}
			if err := cert.SignCert(rand.Reader, ca); err != nil {
				t.Errorf("sign cert: %v", err)
			}
			signed = string(ssh.MarshalAuthorizedKey(cert))
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"signedSshPublicKey": signed})
	}))
	t.Cleanup(srv.Close)
	prev := osLoginBaseURL
	osLoginBaseURL = srv.URL
	t.Cleanup(func() { osLoginBaseURL = prev })
	return s
}

func TestSignInstanceSSHKey_UsesInstanceServiceAccount(t *testing.T) {
	clock := newTestClock()
	srv := startOSLoginServer(t, clock, 0)
	const sa = "agent-runner@my-project.iam.gserviceaccount.com"
	fake := &fakeInstanceGetter{insts: map[string]*runpb.Instance{testInstanceName("agent-1"): {ServiceAccount: sa, Uid: "u1"}}}
	c := newTestConnector(fakeClient(fake), clock)

	key, err := c.signedKeyForInstance(context.Background(), testProject, testLocation, "agent-1", "tok")
	if err != nil {
		t.Fatalf("signedKeyForInstance: %v", err)
	}
	if key.cert != "signed-cert-1" {
		t.Errorf("cert = %q, want %q", key.cert, "signed-cert-1")
	}
	if len(fake.getReqs) != 1 || fake.getReqs[0].GetName() != testInstanceName("agent-1") {
		t.Errorf("GetInstance requests = %v, want one for %q", fake.getReqs, testInstanceName("agent-1"))
	}
	if srv.count() != 1 {
		t.Fatalf("sign requests = %d, want 1", srv.count())
	}
	if got := srv.body(0)["serviceAccount"]; got != sa {
		t.Errorf("sign request serviceAccount = %q, want %q", got, sa)
	}
}

func TestSignInstanceSSHKey_FailsClosedWithoutServiceAccount(t *testing.T) {
	cases := map[string]func(context.Context) (InstancesAPI, error){
		"instance has no service account": fakeClient(&fakeInstanceGetter{insts: map[string]*runpb.Instance{testInstanceName("agent-1"): {Uid: "u1"}}}),
		"get instance fails":              fakeClient(&fakeInstanceGetter{err: errors.New("permission denied")}),
		"client creation fails": func(context.Context) (InstancesAPI, error) {
			return nil, errors.New("no credentials")
		},
	}
	for name, newClient := range cases {
		t.Run(name, func(t *testing.T) {
			clock := newTestClock()
			srv := startOSLoginServer(t, clock, 0)
			c := newTestConnector(newClient, clock)

			_, err := c.signedKeyForInstance(context.Background(), testProject, testLocation, "agent-1", "tok")
			if err == nil {
				t.Fatal("signedKeyForInstance succeeded, want error")
			}
			if !strings.Contains(err.Error(), "failed to determine service account") {
				t.Errorf("error = %q, want it to mention the undetermined service account", err)
			}
			if srv.count() != 0 {
				t.Errorf("sign requests = %d, want 0", srv.count())
			}
			if len(c.signedKeys) != 0 {
				t.Errorf("cached signed keys = %d, want 0", len(c.signedKeys))
			}
		})
	}
}

func TestSignedKey_LooksUpInstanceOnEveryCall(t *testing.T) {
	clock := newTestClock()
	srv := startOSLoginServer(t, clock, 5*time.Minute)
	fake := &fakeInstanceGetter{insts: map[string]*runpb.Instance{testInstanceName("agent-1"): {ServiceAccount: "sa@x", Uid: "u1"}}}
	c := newTestConnector(fakeClient(fake), clock)

	for i := 0; i < 3; i++ {
		if _, err := c.signedKeyForInstance(context.Background(), testProject, testLocation, "agent-1", "tok"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if len(fake.getReqs) != 3 {
		t.Errorf("GetInstance calls = %d, want 3", len(fake.getReqs))
	}
	if srv.count() != 1 {
		t.Errorf("sign requests = %d, want 1", srv.count())
	}
}

func TestSignedKey_ReusedWithinValidityAndResignedAfter(t *testing.T) {
	clock := newTestClock()
	srv := startOSLoginServer(t, clock, 5*time.Minute)
	fake := &fakeInstanceGetter{insts: map[string]*runpb.Instance{testInstanceName("agent-1"): {ServiceAccount: "sa@x", Uid: "u1"}}}
	c := newTestConnector(fakeClient(fake), clock)
	ctx := context.Background()

	first, err := c.signedKeyForInstance(ctx, testProject, testLocation, "agent-1", "tok")
	if err != nil {
		t.Fatalf("first: %v", err)
	}

	// 61 s of validity left: reused.
	clock.advance(5*time.Minute - 61*time.Second)
	reused, err := c.signedKeyForInstance(ctx, testProject, testLocation, "agent-1", "tok")
	if err != nil {
		t.Fatalf("reuse: %v", err)
	}
	if srv.count() != 1 || !bytes.Equal(reused.privKeyPEM, first.privKeyPEM) || reused.cert != first.cert {
		t.Fatalf("within validity: sign requests = %d, key reused = %v; want 1 and true", srv.count(), bytes.Equal(reused.privKeyPEM, first.privKeyPEM))
	}

	// 59 s left: re-signed with a fresh key.
	clock.advance(2 * time.Second)
	fresh, err := c.signedKeyForInstance(ctx, testProject, testLocation, "agent-1", "tok")
	if err != nil {
		t.Fatalf("re-sign: %v", err)
	}
	if srv.count() != 2 {
		t.Errorf("sign requests = %d, want 2", srv.count())
	}
	if bytes.Equal(fresh.privKeyPEM, first.privKeyPEM) || fresh.cert == first.cert {
		t.Error("expired signed key was reused")
	}
}

func TestSignedKey_ExpiryRecheckedAfterLookup(t *testing.T) {
	clock := newTestClock()
	srv := startOSLoginServer(t, clock, 5*time.Minute)
	fake := &fakeInstanceGetter{insts: map[string]*runpb.Instance{testInstanceName("agent-1"): {ServiceAccount: "sa@x", Uid: "u1"}}}
	c := newTestConnector(fakeClient(fake), clock)
	ctx := context.Background()

	first, err := c.signedKeyForInstance(ctx, testProject, testLocation, "agent-1", "tok")
	if err != nil {
		t.Fatalf("first: %v", err)
	}

	// Still reusable when the call starts; the lookup takes 2 s and crosses
	// the reuse deadline.
	clock.advance(5*time.Minute - 61*time.Second)
	fake.onGet = func() { clock.advance(2 * time.Second) }
	second, err := c.signedKeyForInstance(ctx, testProject, testLocation, "agent-1", "tok")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if srv.count() != 2 {
		t.Errorf("sign requests = %d, want 2", srv.count())
	}
	if bytes.Equal(second.privKeyPEM, first.privKeyPEM) || second.cert == first.cert {
		t.Error("key past its reuse deadline after the lookup was reused")
	}
}

func TestSignedKey_SameUIDNewServiceAccountResigns(t *testing.T) {
	clock := newTestClock()
	srv := startOSLoginServer(t, clock, 5*time.Minute)
	fake := &fakeInstanceGetter{insts: map[string]*runpb.Instance{testInstanceName("agent-1"): {ServiceAccount: "old@x", Uid: "u1"}}}
	c := newTestConnector(fakeClient(fake), clock)
	ctx := context.Background()

	first, err := c.signedKeyForInstance(ctx, testProject, testLocation, "agent-1", "tok")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	fake.insts[testInstanceName("agent-1")] = &runpb.Instance{ServiceAccount: "new@x", Uid: "u1"}
	second, err := c.signedKeyForInstance(ctx, testProject, testLocation, "agent-1", "tok")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if srv.count() != 2 {
		t.Fatalf("sign requests = %d, want 2", srv.count())
	}
	if got := srv.body(1)["serviceAccount"]; got != "new@x" {
		t.Errorf("second sign request serviceAccount = %q, want %q", got, "new@x")
	}
	if bytes.Equal(second.privKeyPEM, first.privKeyPEM) || second.cert == first.cert {
		t.Error("key signed for the old service account was reused")
	}
}

func TestSignedKey_DefaultValidityWhenCertUnreadable(t *testing.T) {
	clock := newTestClock()
	srv := startOSLoginServer(t, clock, 0)
	fake := &fakeInstanceGetter{insts: map[string]*runpb.Instance{testInstanceName("agent-1"): {ServiceAccount: "sa@x", Uid: "u1"}}}
	c := newTestConnector(fakeClient(fake), clock)
	ctx := context.Background()

	if _, err := c.signedKeyForInstance(ctx, testProject, testLocation, "agent-1", "tok"); err != nil {
		t.Fatal(err)
	}
	clock.advance(signedKeyDefaultValidity - signedKeyMinRemaining - time.Second)
	if _, err := c.signedKeyForInstance(ctx, testProject, testLocation, "agent-1", "tok"); err != nil {
		t.Fatal(err)
	}
	if srv.count() != 1 {
		t.Fatalf("before default expiry: sign requests = %d, want 1", srv.count())
	}
	clock.advance(2 * time.Second)
	if _, err := c.signedKeyForInstance(ctx, testProject, testLocation, "agent-1", "tok"); err != nil {
		t.Fatal(err)
	}
	if srv.count() != 2 {
		t.Errorf("after default expiry: sign requests = %d, want 2", srv.count())
	}
}

func TestSignedKey_InstancesNeverShareKeyOrCert(t *testing.T) {
	clock := newTestClock()
	srv := startOSLoginServer(t, clock, 5*time.Minute)
	fake := &fakeInstanceGetter{insts: map[string]*runpb.Instance{
		testInstanceName("agent-1"): {ServiceAccount: "sa@x", Uid: "u1"},
		testInstanceName("agent-2"): {ServiceAccount: "sa@x", Uid: "u2"},
	}}
	c := newTestConnector(fakeClient(fake), clock)
	ctx := context.Background()

	a, err := c.signedKeyForInstance(ctx, testProject, testLocation, "agent-1", "tok")
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.signedKeyForInstance(ctx, testProject, testLocation, "agent-2", "tok")
	if err != nil {
		t.Fatal(err)
	}
	// agent-1 re-created under the same name: new uid.
	fake.insts[testInstanceName("agent-1")] = &runpb.Instance{ServiceAccount: "sa@x", Uid: "u3"}
	recreated, err := c.signedKeyForInstance(ctx, testProject, testLocation, "agent-1", "tok")
	if err != nil {
		t.Fatal(err)
	}

	keys := map[string]*signedKey{"agent-1": a, "agent-2": b, "agent-1 re-created": recreated}
	for n1, k1 := range keys {
		for n2, k2 := range keys {
			if n1 >= n2 {
				continue
			}
			if bytes.Equal(k1.privKeyPEM, k2.privKeyPEM) || k1.cert == k2.cert {
				t.Errorf("%s and %s share a key or cert", n1, n2)
			}
		}
	}
	if srv.count() != 3 {
		t.Errorf("sign requests = %d, want 3", srv.count())
	}
	if got := srv.body(1)["cloudRunResource"]; got != testInstanceName("agent-2") {
		t.Errorf("second sign request resource = %q, want %q", got, testInstanceName("agent-2"))
	}
}

func TestSignedKey_ExpiredEntriesSwept(t *testing.T) {
	clock := newTestClock()
	startOSLoginServer(t, clock, 5*time.Minute)
	fake := &fakeInstanceGetter{insts: map[string]*runpb.Instance{
		testInstanceName("agent-1"): {ServiceAccount: "sa@x", Uid: "u1"},
		testInstanceName("agent-2"): {ServiceAccount: "sa@x", Uid: "u2"},
	}}
	c := newTestConnector(fakeClient(fake), clock)
	ctx := context.Background()

	if _, err := c.signedKeyForInstance(ctx, testProject, testLocation, "agent-1", "tok"); err != nil {
		t.Fatal(err)
	}
	clock.advance(5 * time.Minute)
	if _, err := c.signedKeyForInstance(ctx, testProject, testLocation, "agent-2", "tok"); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.signedKeys[testInstanceName("agent-1")]; ok {
		t.Error("expired signed key for agent-1 is still cached")
	}
	if len(c.signedKeys) != 1 {
		t.Errorf("cached signed keys = %d, want 1", len(c.signedKeys))
	}
}

func TestSignedKey_FailedLookupDropsCachedKey(t *testing.T) {
	clock := newTestClock()
	startOSLoginServer(t, clock, 5*time.Minute)
	fake := &fakeInstanceGetter{insts: map[string]*runpb.Instance{testInstanceName("agent-1"): {ServiceAccount: "sa@x", Uid: "u1"}}}
	c := newTestConnector(fakeClient(fake), clock)
	ctx := context.Background()

	if _, err := c.signedKeyForInstance(ctx, testProject, testLocation, "agent-1", "tok"); err != nil {
		t.Fatal(err)
	}
	fake.err = errors.New("not found")
	if _, err := c.signedKeyForInstance(ctx, testProject, testLocation, "agent-1", "tok"); err == nil {
		t.Fatal("signedKeyForInstance succeeded after the lookup failed, want error")
	}
	if len(c.signedKeys) != 0 {
		t.Errorf("cached signed keys = %d, want 0", len(c.signedKeys))
	}
}

func TestForget_DropsCachedKey(t *testing.T) {
	clock := newTestClock()
	srv := startOSLoginServer(t, clock, 5*time.Minute)
	fake := &fakeInstanceGetter{insts: map[string]*runpb.Instance{testInstanceName("agent-1"): {ServiceAccount: "sa@x", Uid: "u1"}}}
	c := newTestConnector(fakeClient(fake), clock)
	ctx := context.Background()

	if _, err := c.signedKeyForInstance(ctx, testProject, testLocation, "agent-1", "tok"); err != nil {
		t.Fatal(err)
	}
	c.Forget(testProject, testLocation, "agent-1")
	if len(c.signedKeys) != 0 {
		t.Fatalf("cached signed keys after Forget = %d, want 0", len(c.signedKeys))
	}
	if _, err := c.signedKeyForInstance(ctx, testProject, testLocation, "agent-1", "tok"); err != nil {
		t.Fatal(err)
	}
	if srv.count() != 2 {
		t.Errorf("sign requests = %d, want 2", srv.count())
	}
}

type fakeTokenSource struct{}

func (fakeTokenSource) Token(context.Context) (*auth.Token, error) {
	return &auth.Token{Value: "tok"}, nil
}

func TestTokenSource_BuiltOnce(t *testing.T) {
	calls := 0
	c := &IAPExecConnector{detectCredentials: func() (tokenSource, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("transient")
		}
		return fakeTokenSource{}, nil
	}}

	if _, err := c.tokenSource(); err == nil {
		t.Fatal("first tokenSource succeeded, want the detection error")
	}
	for i := 0; i < 5; i++ {
		if _, err := c.tokenSource(); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if calls != 2 {
		t.Errorf("credential detections = %d, want 2 (one failed, one cached)", calls)
	}
}

func TestProjectNumber_LookedUpOncePerProject(t *testing.T) {
	lookups := map[string]int{}
	c := &IAPExecConnector{lookupProjectNumber: func(_ context.Context, project string) (string, error) {
		lookups[project]++
		if project == "broken" {
			return "", errors.New("not found")
		}
		return "num-" + project, nil
	}}
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		for _, p := range []string{"p1", "p2"} {
			got, err := c.projectNumber(ctx, p)
			if err != nil || got != "num-"+p {
				t.Fatalf("projectNumber(%q) = %q, %v", p, got, err)
			}
		}
		if _, err := c.projectNumber(ctx, "broken"); err == nil {
			t.Fatal("projectNumber(broken) succeeded, want error")
		}
	}
	if lookups["p1"] != 1 || lookups["p2"] != 1 {
		t.Errorf("lookups = %v, want one per project", lookups)
	}
	if lookups["broken"] != 3 {
		t.Errorf("failed lookups = %d, want 3 (errors are not cached)", lookups["broken"])
	}
}

func TestRunWithAuthRetry(t *testing.T) {
	authErr := fmt.Errorf("SSH command failed: %w: exit status 255", errSSHAuthFailed)
	tests := []struct {
		name         string
		retry        bool
		results      []error
		wantAttempts int
		wantForgot   bool
		wantErr      bool
	}{
		{"success", true, []error{nil}, 1, false, false},
		{"other error", true, []error{errors.New("exit status 1")}, 1, false, true},
		{"auth failure then success", true, []error{authErr, nil}, 2, true, false},
		{"auth failure twice retries once", true, []error{authErr, authErr, nil}, 2, true, true},
		{"auth failure without retry", false, []error{authErr, nil}, 1, true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			name := testInstanceName("agent-1")
			c := &IAPExecConnector{signedKeys: map[string]*signedKey{name: {instanceUID: "u1"}}}
			attempts := 0
			err := c.runWithAuthRetry(testProject, testLocation, "agent-1", tc.retry, func() error {
				attempts++
				return tc.results[attempts-1]
			})
			if attempts != tc.wantAttempts {
				t.Errorf("attempts = %d, want %d", attempts, tc.wantAttempts)
			}
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tc.wantErr)
			}
			_, cached := c.signedKeys[name]
			if cached == tc.wantForgot {
				t.Errorf("signed key cached = %v, want %v", cached, !tc.wantForgot)
			}
		})
	}
}

func TestAuthFailureDetector(t *testing.T) {
	tests := []struct {
		name   string
		writes []string
		want   bool
	}{
		{"one write", []string{"root@127.0.0.1: Permission denied (publickey).\r\n"}, true},
		{"split across writes", []string{"root@127.0.0.1: Permission de", "nied (pub", "lickey).\r\n"}, true},
		{"other failure", []string{"ssh: connect to host 127.0.0.1 port 1: Connection refused\r\n"}, false},
		{"command output", []string{"Permission denied\n"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := &authFailureDetector{}
			for _, w := range tc.writes {
				if n, err := d.Write([]byte(w)); err != nil || n != len(w) {
					t.Fatalf("Write = %d, %v", n, err)
				}
			}
			if d.Seen() != tc.want {
				t.Errorf("Seen = %v, want %v", d.Seen(), tc.want)
			}
		})
	}
}

func TestAuthRetryPerMethod(t *testing.T) {
	authErr := fmt.Errorf("SSH command failed: %w: exit status 255", errSSHAuthFailed)
	tests := []struct {
		name         string
		call         func(*IAPExecConnector) ([]byte, error)
		wantAttempts int
		wantOut      string
		wantErr      bool
	}{
		{"Connect retries", func(c *IAPExecConnector) ([]byte, error) {
			return nil, c.Connect(context.Background(), testProject, testLocation, "agent-1")
		}, 2, "", false},
		{"Exec retries with fresh output", func(c *IAPExecConnector) ([]byte, error) {
			return c.Exec(context.Background(), testProject, testLocation, "agent-1", []string{"true"})
		}, 2, "ok", false},
		{"ExecWithStdin does not retry", func(c *IAPExecConnector) ([]byte, error) {
			return c.ExecWithStdin(context.Background(), testProject, testLocation, "agent-1", []string{"cat"}, strings.NewReader("secret"))
		}, 1, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			name := testInstanceName("agent-1")
			c := &IAPExecConnector{signedKeys: map[string]*signedKey{name: {instanceUID: "u1"}}}
			attempts := 0
			c.sshRunner = func(_ context.Context, _, _, _ string, _ []string, _ io.Reader, stdout, stderr io.Writer) error {
				attempts++
				if attempts == 1 {
					_, _ = io.WriteString(stderr, "root@127.0.0.1: Permission denied (publickey).\r\n")
					return authErr
				}
				_, _ = io.WriteString(stdout, "ok")
				return nil
			}

			out, err := tc.call(c)
			if attempts != tc.wantAttempts {
				t.Errorf("attempts = %d, want %d", attempts, tc.wantAttempts)
			}
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantOut != "" && string(out) != tc.wantOut {
				t.Errorf("output = %q, want %q", out, tc.wantOut)
			}
			if _, cached := c.signedKeys[name]; cached {
				t.Error("signed key still cached after the authentication failure")
			}
		})
	}
}

func TestInstancesClient_BuiltOnce(t *testing.T) {
	clock := newTestClock()
	startOSLoginServer(t, clock, 5*time.Minute)
	fake := &fakeInstanceGetter{insts: map[string]*runpb.Instance{testInstanceName("agent-1"): {ServiceAccount: "sa@x", Uid: "u1"}}}
	builds := 0
	c := newTestConnector(func(context.Context) (InstancesAPI, error) {
		builds++
		if builds == 1 {
			return nil, errors.New("transient")
		}
		return fake, nil
	}, clock)
	ctx := context.Background()

	if _, err := c.signedKeyForInstance(ctx, testProject, testLocation, "agent-1", "tok"); err == nil {
		t.Fatal("signedKeyForInstance succeeded with no client, want error")
	}
	for i := 0; i < 3; i++ {
		if _, err := c.signedKeyForInstance(ctx, testProject, testLocation, "agent-1", "tok"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if builds != 2 {
		t.Errorf("client builds = %d, want 2 (one failed, one cached)", builds)
	}
	if fake.closeCount != 0 {
		t.Errorf("client closed %d times during calls, want 0", fake.closeCount)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if fake.closeCount != 1 {
		t.Errorf("client closed %d times by Close, want 1", fake.closeCount)
	}
}

// TestSignedKey_Concurrent exercises signedKeyForInstance, Forget and the
// expiry sweep on the same instance from several goroutines. Run it with
// -race.
func TestSignedKey_Concurrent(t *testing.T) {
	clock := newTestClock()
	startOSLoginServer(t, clock, 5*time.Minute)
	fake := &fakeInstanceGetter{insts: map[string]*runpb.Instance{
		testInstanceName("agent-1"): {ServiceAccount: "sa@x", Uid: "u1"},
		testInstanceName("agent-2"): {ServiceAccount: "sa@x", Uid: "u2"},
	}}
	c := newTestConnector(fakeClient(fake), clock)
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			shortID := "agent-1"
			if w%2 == 1 {
				shortID = "agent-2"
			}
			for i := 0; i < 5; i++ {
				k, err := c.signedKeyForInstance(ctx, testProject, testLocation, shortID, "tok")
				if err != nil {
					errs <- err
					return
				}
				if len(k.privKeyPEM) == 0 || k.cert == "" {
					errs <- fmt.Errorf("worker %d got an empty signed key", w)
					return
				}
			}
		}(w)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 5; i++ {
			c.Forget(testProject, testLocation, "agent-1")
			// Push cached keys past their reuse deadline so the next
			// call sweeps them.
			clock.advance(2 * time.Minute)
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
