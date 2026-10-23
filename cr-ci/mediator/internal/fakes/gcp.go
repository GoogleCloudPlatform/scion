package fakes

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// ---- Cloud Run Admin API v2 ------------------------------------------------

// Execution is an execution in the fake Cloud Run.
type Execution struct {
	Name      string
	Created   time.Time
	Completed bool
	Cancelled bool
	Env       map[string]string
}

// CloudRun is a fake of the Cloud Run Admin API v2 jobs and executions
// endpoints for a single job.
type CloudRun struct {
	Server  *httptest.Server
	JobName string // projects/p/locations/r/jobs/j
	Now     func() time.Time

	mu         sync.Mutex
	executions []*Execution
	seq        int

	FailRun     bool          // jobs.run answers 429 RESOURCE_EXHAUSTED
	OnRun       func()        // called when jobs.run is received (before it is processed)
	ListLag     time.Duration // executions younger than this are not listed yet
	RunBodies   []string
	CancelCalls []string
	ListCalls   int
	Tokens      []string // bearer tokens received
}

// NewCloudRun starts a fake for job jobName.
func NewCloudRun(jobName string) *CloudRun {
	c := &CloudRun{JobName: jobName, Now: time.Now}
	c.Server = httptest.NewServer(http.HandlerFunc(c.serve))
	return c
}

// Close stops the server.
func (c *CloudRun) Close() { c.Server.Close() }

// AddExecution adds an execution (for example a running one from earlier).
func (c *CloudRun) AddExecution(e Execution) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e.Name == "" {
		c.seq++
		e.Name = fmt.Sprintf("%s/executions/pre-%d", c.JobName, c.seq)
	}
	cp := e
	c.executions = append(c.executions, &cp)
}

// Executions returns a snapshot of all executions.
func (c *CloudRun) Executions() []Execution {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Execution
	for _, e := range c.executions {
		out = append(out, *e)
	}
	return out
}

// Complete marks every execution for GH_JOB_ID jobID as completed.
func (c *CloudRun) Complete(jobID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.executions {
		if e.Env["GH_JOB_ID"] == jobID {
			e.Completed = true
		}
	}
}

func (c *CloudRun) serve(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Tokens = append(c.Tokens, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	path := strings.TrimPrefix(r.URL.Path, "/v2/")
	switch {
	case r.Method == http.MethodPost && path == c.JobName+":run":
		if c.OnRun != nil {
			c.OnRun()
		}
		var body struct {
			Overrides struct {
				ContainerOverrides []struct {
					Env []struct{ Name, Value string } `json:"env"`
				} `json:"containerOverrides"`
			} `json:"overrides"`
		}
		raw := new(strings.Builder)
		dec := json.NewDecoder(teeReader{r: r.Body, w: raw})
		if err := dec.Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
			return
		}
		c.RunBodies = append(c.RunBodies, raw.String())
		if c.FailRun {
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": map[string]any{"code": 429, "status": "RESOURCE_EXHAUSTED"}})
			return
		}
		c.seq++
		e := &Execution{Name: fmt.Sprintf("%s/executions/exec-%d", c.JobName, c.seq), Created: c.Now(), Env: map[string]string{}}
		for _, co := range body.Overrides.ContainerOverrides {
			for _, v := range co.Env {
				e.Env[v.Name] = v.Value
			}
		}
		c.executions = append(c.executions, e)
		writeJSON(w, http.StatusOK, map[string]any{
			"name":     "projects/p/locations/r/operations/op-1",
			"metadata": map[string]any{"@type": "type.googleapis.com/google.cloud.run.v2.Execution", "name": e.Name},
		})
	case r.Method == http.MethodGet && path == c.JobName+"/executions":
		c.ListCalls++
		var list []any
		for _, e := range c.executions {
			if c.ListLag > 0 && e.Created.After(c.Now().Add(-c.ListLag)) {
				continue
			}
			m := map[string]any{"name": e.Name, "createTime": e.Created.UTC().Format(time.RFC3339Nano)}
			if e.Completed {
				m["completionTime"] = e.Created.Add(time.Minute).UTC().Format(time.RFC3339Nano)
			}
			var env []any
			for k, v := range e.Env {
				env = append(env, map[string]string{"name": k, "value": v})
			}
			m["template"] = map[string]any{"containers": []any{map[string]any{"env": env}}}
			list = append(list, m)
		}
		writeJSON(w, http.StatusOK, map[string]any{"executions": list})
	case r.Method == http.MethodPost && strings.HasSuffix(path, ":cancel"):
		name := strings.TrimSuffix(path, ":cancel")
		c.CancelCalls = append(c.CancelCalls, name)
		for _, e := range c.executions {
			if e.Name == name {
				if e.Completed {
					writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"code": 400, "status": "FAILED_PRECONDITION"}})
					return
				}
				e.Completed, e.Cancelled = true, true
				writeJSON(w, http.StatusOK, map[string]any{"name": "op-cancel"})
				return
			}
		}
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": 404, "status": "NOT_FOUND"}})
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "unexpected " + r.Method + " " + r.URL.Path})
	}
}

type teeReader struct {
	r interface{ Read([]byte) (int, error) }
	w *strings.Builder
}

func (t teeReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	t.w.Write(p[:n])
	return n, err
}

// ---- metadata server and Secret Manager -------------------------------------

// GoogleAPIs fakes the metadata server token endpoint and Secret Manager.
type GoogleAPIs struct {
	// SecretGate, if set: each secret access sends on SecretStarted, then
	// waits for SecretRelease to be closed before answering.
	SecretStarted chan struct{}
	SecretRelease chan struct{}

	Server  *httptest.Server
	Project string

	mu           sync.Mutex
	secrets      map[string][]byte
	seq          int
	TokenFetches int
	Accesses     map[string]int
	Misses       map[string]int
	IssuedTokens []string
}

// NewGoogleAPIs starts the fake with the given secret payloads.
func NewGoogleAPIs(project string, secrets map[string][]byte) *GoogleAPIs {
	g := &GoogleAPIs{Project: project, secrets: map[string][]byte{}, Accesses: map[string]int{}, Misses: map[string]int{}}
	for k, v := range secrets {
		g.secrets[k] = v
	}
	g.Server = httptest.NewServer(http.HandlerFunc(g.serve))
	return g
}

// Close stops the server.
func (g *GoogleAPIs) Close() { g.Server.Close() }

// SetSecret adds a new latest version of secret name.
func (g *GoogleAPIs) SetSecret(name string, v []byte) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.secrets[name] = v
}

// SetSecretGate installs the secret access gate (see SecretStarted).
func (g *GoogleAPIs) SetSecretGate(started, release chan struct{}) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.SecretStarted, g.SecretRelease = started, release
}

// DeleteSecret removes secret name.
func (g *GoogleAPIs) DeleteSecret(name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.secrets, name)
}

// AccessCount and MissCount report successful and not-found accesses.
func (g *GoogleAPIs) AccessCount(name string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.Accesses[name]
}

// MissCount reports not-found accesses of secret name.
func (g *GoogleAPIs) MissCount(name string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.Misses[name]
}

func (g *GoogleAPIs) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if r.URL.Path == "/computeMetadata/v1/instance/service-accounts/default/token" {
		if r.Header.Get("Metadata-Flavor") != "Google" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		g.seq++
		g.TokenFetches++
		tok := fmt.Sprintf("ya29.FAKEaccesstoken%04d", g.seq)
		g.IssuedTokens = append(g.IssuedTokens, tok)
		writeJSON(w, http.StatusOK, map[string]any{"access_token": tok, "expires_in": 3599, "token_type": "Bearer"})
		return
	}
	prefix := "/v1/projects/" + g.Project + "/secrets/"
	if name, ok := strings.CutPrefix(r.URL.Path, prefix); ok && strings.HasSuffix(name, "/versions/latest:access") {
		if started, release := g.SecretStarted, g.SecretRelease; started != nil {
			g.mu.Unlock()
			started <- struct{}{}
			<-release
			g.mu.Lock()
		}
		name = strings.TrimSuffix(name, "/versions/latest:access")
		auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		known := false
		for _, t := range g.IssuedTokens {
			known = known || t == auth
		}
		if !known {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{"code": 401}})
			return
		}
		v, ok := g.secrets[name]
		if !ok {
			g.Misses[name]++
			writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": 404}})
			return
		}
		g.Accesses[name]++
		writeJSON(w, http.StatusOK, map[string]any{
			"name":    "projects/" + g.Project + "/secrets/" + name + "/versions/1",
			"payload": map[string]any{"data": base64.StdEncoding.EncodeToString(v)},
		})
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]any{"error": "unexpected " + r.URL.Path})
}

// Secrets returns every access token the fake issued.
func (g *GoogleAPIs) Secrets() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.IssuedTokens...)
}

// ---- Google ID tokens --------------------------------------------------------

// Google signs ID tokens with a throwaway key and serves the matching JWKS.
type Google struct {
	Server *httptest.Server
	Key    *rsa.PrivateKey
	Kid    string

	mu      sync.Mutex
	fetches int  // JWKS requests served; read with Fetches()
	Fail    bool // JWKS endpoint answers 500
	// Gate, if set: each JWKS request sends on Started, then waits for
	// Release to be closed before answering.
	Started chan struct{}
	Release chan struct{}
}

// Fetches returns how many JWKS requests the server has answered (under the
// lock, so tests can read it while handlers run).
func (g *Google) Fetches() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.fetches
}

// NewGoogle starts a JWKS server for key.
func NewGoogle(key *rsa.PrivateKey) *Google {
	g := &Google{Key: key, Kid: "fake-kid-1"}
	g.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		g.fetches++
		fail, started, release, kid := g.Fail, g.Started, g.Release, g.Kid
		g.mu.Unlock()
		if started != nil {
			started <- struct{}{}
			<-release
		}
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=3600")
		writeJSON(w, http.StatusOK, map[string]any{"keys": []any{map[string]string{
			"kid": kid, "kty": "RSA", "alg": "RS256", "use": "sig",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	return g
}

// Close stops the server.
func (g *Google) Close() { g.Server.Close() }

// GateBackstop bounds how long a test waits for work that must finish while a
// gated fake request is held. On a pass the work takes milliseconds; the bound
// only turns a regression (the work waiting for the gate, which is released
// after the wait) into a failure instead of a hang, so it can be generous.
const GateBackstop = 30 * time.Second

// SetKid changes the kid the JWKS serves and IDToken signs with (key rotation).
func (g *Google) SetKid(kid string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.Kid = kid
}

// SetFail makes the JWKS endpoint answer 500 (true) or normally (false).
func (g *Google) SetFail(fail bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.Fail = fail
}

// SetGate installs the JWKS request gate (see Started and Release).
func (g *Google) SetGate(started, release chan struct{}) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.Started, g.Release = started, release
}

// Mint returns an ID token with claims, signed by key under kid.
func Mint(key *rsa.PrivateKey, kid string, claims map[string]any) string {
	h, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": kid, "typ": "JWT"})
	c, _ := json.Marshal(claims)
	in := base64.RawURLEncoding.EncodeToString(h) + "." + base64.RawURLEncoding.EncodeToString(c)
	d := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, d[:])
	if err != nil {
		panic(err)
	}
	return in + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// IDToken returns a valid-looking Google ID token for aud and email.
func (g *Google) IDToken(aud, email string, now time.Time) string {
	g.mu.Lock()
	kid := g.Kid
	g.mu.Unlock()
	return Mint(g.Key, kid, map[string]any{
		"iss": "https://accounts.google.com", "aud": aud, "email": email, "email_verified": true,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(), "sub": "1234567890",
	})
}

// ---- keys --------------------------------------------------------------------

var (
	keyMu   sync.Mutex
	keyPool []*rsa.PrivateKey
)

// Key returns throwaway 2048-bit RSA key number i, generated on first use.
func Key(i int) *rsa.PrivateKey {
	keyMu.Lock()
	defer keyMu.Unlock()
	for len(keyPool) <= i {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			panic(err)
		}
		keyPool = append(keyPool, k)
	}
	return keyPool[i]
}

// PEM encodes key as PKCS#1 PEM, the format GitHub issues.
func PEM(key *rsa.PrivateKey) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}
