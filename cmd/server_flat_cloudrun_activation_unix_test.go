//go:build unix && !no_sqlite

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

package cmd

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"

	"cloud.google.com/go/auth"
	"cloud.google.com/go/auth/credentials"
	resourcemanagerpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokercredentials"
	"github.com/GoogleCloudPlatform/scion/pkg/brokerhost"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/brokerregistration"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
)

const (
	crNumber     = "123456789012"
	crAgentSA    = "agents@example-project.iam.gserviceaccount.com"
	crPrincipalA = "broker-a@example-project.iam.gserviceaccount.com"
	crPrincipalB = "broker-b@example-project.iam.gserviceaccount.com"
	crHubURL     = "https://hub.example.com"
)

var (
	crKeyOnce sync.Once
	crKeyPEM  string
	crKeyErr  error
)

// crTestKey is a PKCS#8 RSA private key in PEM, generated once per test binary,
// so fixture key files parse as real service-account keys.
func crTestKey(t *testing.T) string {
	t.Helper()
	crKeyOnce.Do(func() {
		k, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			crKeyErr = err
			return
		}
		der, err := x509.MarshalPKCS8PrivateKey(k)
		if err != nil {
			crKeyErr = err
			return
		}
		crKeyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	})
	require.NoError(t, crKeyErr)
	return crKeyPEM
}

func crSAKey(t *testing.T, email string) map[string]any {
	return map[string]any{
		"type": "service_account", "project_id": "example-project", "private_key_id": "0123456789abcdef",
		"private_key": crTestKey(t), "client_email": email, "client_id": "100000000000000000001",
		"token_uri": "https://oauth2.googleapis.com/token",
	}
}

func crSAKeyJSON(t *testing.T, email string) []byte {
	b, err := json.Marshal(crSAKey(t, email))
	require.NoError(t, err)
	return b
}

func crImpersonatedJSON(t *testing.T, target string, source map[string]any) []byte {
	b, err := json.Marshal(map[string]any{
		"type":                              "impersonated_service_account",
		"service_account_impersonation_url": "https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/" + target + ":generateAccessToken",
		"source_credentials":                source,
		"delegates":                         []string{},
	})
	require.NoError(t, err)
	return b
}

// crWriteFile writes data at dir/name with mode and returns its path.
func crWriteFile(t *testing.T, dir, name string, data []byte, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, data, mode))
	require.NoError(t, os.Chmod(p, mode))
	return p
}

// crHostFacts is a host on which every cloudrun precondition passes.
func crHostFacts() flatPrecheckHost {
	return flatPrecheckHost{
		mode:           brokerhost.ModeColocated,
		layout:         projectfs.Layout{Backend: projectfs.BackendNFS, Homes: projectfs.BackendNFS},
		hubEndpoint:    crHubURL,
		hubEndpointSrc: hubEndpointSourceConfig,
		getenv:         func(string) string { return "" },
	}
}

// crInst is a cloudrun instance in region whose credentials file is path.
func crInst(key, region, path string) config.V1RuntimeBrokerInstanceConfig {
	t := flatCRTarget(region)
	t.CredentialsFile = path
	return config.V1RuntimeBrokerInstanceConfig{Key: key, Name: key, RuntimeTarget: t}
}

// crFakeRuntime stands in for a flat *runtime.CloudRunRuntime in command
// tests: a runtime named "cloudrun" (runtime.MockRuntime) that implements
// flatCloudRunScopeRuntime with canned probe reads. Production code has no
// exported way to build a flat runtime without its own credentials; the
// runtime's own binding and client behaviour is tested in pkg/runtime.
type crFakeRuntime struct {
	*runtime.MockRuntime
	cfg        config.CloudRunConfig
	project    func() (*resourcemanagerpb.Project, error)
	listErr    error
	bound      string
	probeCalls int
}

func newCRFakeRuntime(cfg config.CloudRunConfig, project func() (*resourcemanagerpb.Project, error), listErr error) *crFakeRuntime {
	return &crFakeRuntime{MockRuntime: &runtime.MockRuntime{NameFunc: func() string { return "cloudrun" }}, cfg: cfg, project: project, listErr: listErr}
}

func (f *crFakeRuntime) ProbeProject(context.Context) (*resourcemanagerpb.Project, error) {
	f.probeCalls++
	return f.project()
}

func (f *crFakeRuntime) ProbeListInstances(context.Context) error { return f.listErr }

func (f *crFakeRuntime) BindProjectNumber(n string) error {
	switch {
	case n == "":
		return errors.New("empty number")
	case f.bound == "" || f.bound == n:
		f.bound = n
		return nil
	default:
		return runtime.ErrProjectNumberBound
	}
}

func (f *crFakeRuntime) ProjectID() string { return f.cfg.ProjectID }
func (f *crFakeRuntime) Location() string  { return f.cfg.Location }

// crRig drives a flat host with the real activation (precondition, factory
// checks, probe), fake Cloud Run and Resource Manager clients, and a docker
// sibling whose scope is faked.
type crRig struct {
	t   *testing.T
	act *flatInstanceActivation
	rec *recordingFlatActivator

	mu           sync.Mutex
	runtimeCalls map[string]int
	projectCalls int
	hubCalls     int
	built        []*auth.Credentials
	logs         []string

	project    *resourcemanagerpb.Project
	projectErr error
	listErr    error
	hubID      string
	hubErr     error
}

func newCRRig(t *testing.T, hc flatPrecheckHost) *crRig {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	r := &crRig{t: t, rec: &recordingFlatActivator{}, runtimeCalls: map[string]int{},
		project: &resourcemanagerpb.Project{Name: "projects/" + crNumber, State: resourcemanagerpb.Project_ACTIVE}}
	r.act = newFlatInstanceActivation(hc)
	r.act.hubPrincipal = func(context.Context) (string, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.hubCalls++
		return r.hubID, r.hubErr
	}
	r.act.logf = func(format string, args ...any) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.logs = append(r.logs, fmt.Sprintf(format, args...))
	}
	r.act.newCloudRunRuntime = func(cfg config.CloudRunConfig, creds *auth.Credentials) (runtime.Runtime, error) {
		r.mu.Lock()
		r.built = append(r.built, creds)
		r.mu.Unlock()
		return newCRFakeRuntime(cfg, func() (*resourcemanagerpb.Project, error) { return r.getProject(context.Background(), "") }, r.listErr), nil
	}
	return r
}

func (r *crRig) getProject(context.Context, string) (*resourcemanagerpb.Project, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.projectCalls++
	return r.project, r.projectErr
}

// prepare runs a host over insts and returns its status by key.
func (r *crRig) prepare(insts ...config.V1RuntimeBrokerInstanceConfig) map[string]brokerhost.InstanceStatus {
	r.t.Helper()
	h, err := brokerhost.New(brokerhost.Config{
		GlobalDir:    r.t.TempDir(),
		Instances:    insts,
		Mode:         brokerhost.ModeColocated,
		Precondition: r.act.precondition,
		NewRuntime: func(ctx context.Context, inst config.V1RuntimeBrokerInstanceConfig) (runtime.Runtime, error) {
			r.mu.Lock()
			r.runtimeCalls[inst.Key]++
			r.mu.Unlock()
			return r.act.newRuntime(ctx, inst)
		},
		ProbeScope: func(ctx context.Context, inst config.V1RuntimeBrokerInstanceConfig, rt runtime.Runtime) (brokeridentity.ExecutionScope, error) {
			if inst.RuntimeTarget.Type == config.RuntimeTargetTypeDocker {
				return brokeridentity.ExecutionScope{Type: "docker", Docker: &brokeridentity.DockerScope{DaemonID: "daemon-1"}}, nil
			}
			return probeFlatInstanceScope(ctx, inst, rt)
		},
		Activator: r.rec,
		BuildServer: func(brokerhost.InstanceContext) (*runtimebroker.Server, error) {
			return nil, errors.New("not serving in this test")
		},
	})
	require.NoError(r.t, err)
	require.NoError(r.t, h.Prepare(context.Background()))
	out := map[string]brokerhost.InstanceStatus{}
	for _, s := range h.Status() {
		out[s.Key] = s
	}
	return out
}

func (r *crRig) activated(key string) bool {
	r.rec.mu.Lock()
	defer r.rec.mu.Unlock()
	_, ok := r.rec.activated[key]
	return ok
}

var crDocker = config.V1RuntimeBrokerInstanceConfig{Key: "docker-a", Name: "d", RuntimeTarget: &config.V1RuntimeTargetConfig{Type: config.RuntimeTargetTypeDocker}}

// crPreconditionCase sets up one P1-P6 refusal: it may change the host facts
// and returns the instance's credentials path.
type crPreconditionCase struct {
	name   string
	reason string
	setup  func(t *testing.T, hc *flatPrecheckHost, dir string) string
}

func crGoodCredentials(t *testing.T, hc *flatPrecheckHost, dir string) string {
	return crWriteFile(t, dir, "cr.json", crSAKeyJSON(t, crPrincipalA), 0o600)
}

var crPreconditionCases = []crPreconditionCase{
	{"remote mode", reasonCloudRunRemoteUnsupported, func(t *testing.T, hc *flatPrecheckHost, dir string) string {
		hc.mode = brokerhost.ModeRemote
		return crGoodCredentials(t, hc, dir)
	}},
	{"on a Cloud Run service", reasonCloudRunHostOnCloudRun, func(t *testing.T, hc *flatPrecheckHost, dir string) string {
		hc.getenv = func(name string) string {
			if name == "K_SERVICE" {
				return "hub-service"
			}
			return ""
		}
		return crGoodCredentials(t, hc, dir)
	}},
	{"non-nfs backend", reasonCloudRunStorageBackend, func(t *testing.T, hc *flatPrecheckHost, dir string) string {
		hc.layout = projectfs.Layout{Backend: projectfs.BackendLocal, Homes: projectfs.BackendNFS}
		return crGoodCredentials(t, hc, dir)
	}},
	{"storage settings unusable", reasonCloudRunStorageBackend, func(t *testing.T, hc *flatPrecheckHost, dir string) string {
		hc.storageErr = errors.New("invalid project storage settings")
		return crGoodCredentials(t, hc, dir)
	}},
	{"homes not nfs", reasonCloudRunHomesPlacement, func(t *testing.T, hc *flatPrecheckHost, dir string) string {
		hc.layout = projectfs.Layout{Backend: projectfs.BackendNFS, Homes: projectfs.BackendLocal}
		return crGoodCredentials(t, hc, dir)
	}},
	{"loopback hub endpoint", reasonCloudRunHubEndpoint, func(t *testing.T, hc *flatPrecheckHost, dir string) string {
		hc.hubEndpoint = "https://localhost:8443"
		return crGoodCredentials(t, hc, dir)
	}},
	{"IAP-derived hub endpoint", reasonCloudRunHubEndpoint, func(t *testing.T, hc *flatPrecheckHost, dir string) string {
		hc.hubEndpointSrc = hubEndpointSourceIAPAudience
		return crGoodCredentials(t, hc, dir)
	}},
	{"plain http hub endpoint", reasonCloudRunHubEndpoint, func(t *testing.T, hc *flatPrecheckHost, dir string) string {
		hc.hubEndpoint = "http://hub.example.com"
		return crGoodCredentials(t, hc, dir)
	}},
	{"credentials file mode 0644", reasonCloudRunCredentialsFile, func(t *testing.T, hc *flatPrecheckHost, dir string) string {
		return crWriteFile(t, dir, "cr.json", crSAKeyJSON(t, crPrincipalA), 0o644)
	}},
	{"credentials file symlink", reasonCloudRunCredentialsFile, func(t *testing.T, hc *flatPrecheckHost, dir string) string {
		target := crWriteFile(t, dir, "real.json", crSAKeyJSON(t, crPrincipalA), 0o600)
		link := filepath.Join(dir, "cr.json")
		require.NoError(t, os.Symlink(target, link))
		return link
	}},
}

// crRunPreconditionCase runs one P1-P6 refusal through a host with a docker
// sibling and checks the refusal comes before any runtime, credential
// parse, cloud or metadata call, with its reason code.
func crRunPreconditionCase(t *testing.T, tc crPreconditionCase) {
	t.Helper()
	hc := crHostFacts()
	dir := t.TempDir()
	path := tc.setup(t, &hc, dir)
	r := newCRRig(t, hc)
	st := r.prepare(crDocker, crInst("cr-a", "us-central1", path))
	assert.Equal(t, brokerhost.StateRefused, st["cr-a"].State)
	assert.Equal(t, tc.reason, st["cr-a"].Reason)
	assert.Zero(t, r.runtimeCalls["cr-a"], "no NewRuntime for a refused precondition")
	assert.Empty(t, r.built, "no credential is built")
	assert.Zero(t, r.projectCalls, "no cloud call")
	assert.Zero(t, r.hubCalls, "no metadata call")
	assert.NotContains(t, st["cr-a"].Error, dir, "the refusal names no credentials path")
	assert.True(t, r.activated("docker-a"), "the docker sibling activates")
}

// TestFlatHost_CloudRunPreconditionsNoCloudCall: P1-P6 each refuse only the
// cloudrun instance, with its reason code, before the runtime is built,
// credentials are read, and before any cloud or metadata call.
func TestFlatHost_CloudRunPreconditionsNoCloudCall(t *testing.T) {
	for _, tc := range crPreconditionCases {
		t.Run(tc.name, func(t *testing.T) { crRunPreconditionCase(t, tc) })
	}
}

func crCase(t *testing.T, name string) crPreconditionCase {
	for _, tc := range crPreconditionCases {
		if tc.name == name {
			return tc
		}
	}
	t.Fatalf("no precondition case %q", name)
	return crPreconditionCase{}
}

func TestFlatHost_CloudRunRefusedInRemoteMode(t *testing.T) {
	crRunPreconditionCase(t, crCase(t, "remote mode"))
}

func TestFlatHost_CloudRunRefusedOnCloudRunService(t *testing.T) {
	crRunPreconditionCase(t, crCase(t, "on a Cloud Run service"))
}

func TestFlatHost_CloudRunRefusedNonNFSBackend(t *testing.T) {
	crRunPreconditionCase(t, crCase(t, "non-nfs backend"))
}

func TestFlatHost_CloudRunRefusedHomesNotNFS(t *testing.T) {
	crRunPreconditionCase(t, crCase(t, "homes not nfs"))
}

func TestFlatHost_CloudRunRefusedLoopbackHubEndpoint(t *testing.T) {
	crRunPreconditionCase(t, crCase(t, "loopback hub endpoint"))
}

func TestFlatHost_CloudRunRefusedIAPDerivedEndpoint(t *testing.T) {
	crRunPreconditionCase(t, crCase(t, "IAP-derived hub endpoint"))
}

func TestFlatHost_CloudRunRefusedHTTPHubEndpoint(t *testing.T) {
	crRunPreconditionCase(t, crCase(t, "plain http hub endpoint"))
}

// TestFlatHost_CloudRunHubEndpointForms: P5 refuses link-local IPv4 and
// IPv6, a non-absolute URL, a non-http(s) scheme, plain http, loopback and
// an IAP-derived source; the value P5 accepts is exactly the one applied to
// the instance's agents.
func TestFlatHost_CloudRunHubEndpointForms(t *testing.T) {
	for _, ep := range []string{"https://169.254.10.1", "https://[fe80::2]", "hub.example.com", "ftp://hub.example.com",
		"http://hub.example.com", "https://127.0.0.1:8443", "https://localhost", ""} {
		_, err := flatCloudRunAgentHubEndpoint(ep, hubEndpointSourceConfig)
		assert.Error(t, err, ep)
		assert.Empty(t, flatCloudRunContainerHub(ep, hubEndpointSourceConfig).Endpoint, ep)
	}
	_, err := flatCloudRunAgentHubEndpoint(crHubURL, hubEndpointSourceIAPAudience)
	assert.Error(t, err, "an IAP-derived source is refused")

	got, err := flatCloudRunAgentHubEndpoint(crHubURL+"/", hubEndpointSourceBaseURLEnv)
	require.NoError(t, err)
	assert.Equal(t, crHubURL, got)
	applied := flatCloudRunContainerHub(crHubURL+"/", hubEndpointSourceBaseURLEnv)
	assert.Equal(t, containerHubEndpointResult{Endpoint: got}, applied, "the applied value is the P5 value")
}

// TestFlatCloudRunCredentials_FileChecks: P6 refuses a symlink, a mode
// other than 0600/0400, a non-regular file (directory, FIFO), a missing
// file and a file owned by another user, each in the Precondition hook
// before any parse, without naming the path; 0600 and 0400 pass.
func TestFlatCloudRunCredentials_FileChecks(t *testing.T) {
	dir := t.TempDir()
	key := crSAKeyJSON(t, crPrincipalA)
	refused := map[string]string{
		"mode 0644": crWriteFile(t, dir, "m644.json", key, 0o644),
		"mode 0640": crWriteFile(t, dir, "m640.json", key, 0o640),
		"mode 0700": crWriteFile(t, dir, "m700.json", key, 0o700),
		"missing":   filepath.Join(dir, "missing.json"),
	}
	link := filepath.Join(dir, "link.json")
	require.NoError(t, os.Symlink(crWriteFile(t, dir, "target.json", key, 0o600), link))
	refused["symlink"] = link
	sub := filepath.Join(dir, "dir.json")
	require.NoError(t, os.Mkdir(sub, 0o700))
	refused["directory"] = sub
	fifo := filepath.Join(dir, "fifo.json")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))
	refused["fifo"] = fifo
	if os.Geteuid() == 0 {
		other := crWriteFile(t, dir, "other.json", key, 0o600)
		require.NoError(t, os.Chown(other, 65534, 65534))
		refused["owned by another user"] = other
	} else {
		t.Log("not root: the other-owner case cannot be set up here; privatefs.CheckPrivate covers it")
	}
	for name, p := range refused {
		t.Run(name, func(t *testing.T) {
			_, err := flatInstancePrecheck(context.Background(), crInst("cr-a", "us-central1", p), crHostFacts())
			require.Error(t, err)
			assert.Equal(t, reasonCloudRunCredentialsFile, brokerhost.ReasonCode(err))
			assert.NotContains(t, err.Error(), dir, "the refusal names no path")
		})
	}
	for _, mode := range []os.FileMode{0o600, 0o400} {
		p := crWriteFile(t, dir, fmt.Sprintf("ok%o.json", mode), key, mode)
		got, err := flatInstancePrecheck(context.Background(), crInst("cr-a", "us-central1", p), crHostFacts())
		require.NoError(t, err, "mode %o", mode)
		assert.Equal(t, key, got.data)
	}
}

// TestFlatCloudRunCredentials_ParsedFromCheckedHandle (amendment P5-CR, CR3
// credentials, checks P6 and P7): a file swapped
// at the path after the precondition is not the one parsed: the bytes read
// from the checked handle are used.
func TestFlatCloudRunCredentials_ParsedFromCheckedHandle(t *testing.T) {
	dir := t.TempDir()
	path := crWriteFile(t, dir, "cr.json", crSAKeyJSON(t, crPrincipalA), 0o600)
	act := newFlatInstanceActivation(crHostFacts())
	act.hubPrincipal = func(context.Context) (string, error) { return "", nil }
	act.logf = func(string, ...any) {}
	var parsed []byte
	act.buildCredentials = func(_ credentials.CredType, data []byte) (*auth.Credentials, error) {
		parsed = append([]byte(nil), data...)
		return flatCloudRunCredentialsFromJSON(credentials.ServiceAccount, data)
	}
	act.newCloudRunRuntime = func(cfg config.CloudRunConfig, _ *auth.Credentials) (runtime.Runtime, error) {
		return newCRFakeRuntime(cfg, func() (*resourcemanagerpb.Project, error) { return crActive, nil }, nil), nil
	}
	inst := crInst("cr-a", "us-central1", path)
	require.NoError(t, act.precondition(context.Background(), inst))

	// Swap the file at the path: a different, unaccepted credential.
	require.NoError(t, os.Remove(path))
	crWriteFile(t, dir, "cr.json", []byte(`{"type":"authorized_user","client_id":"x","client_secret":"y","refresh_token":"z"}`), 0o600)

	_, err := act.newRuntime(context.Background(), inst)
	require.NoError(t, err, "the checked bytes (an accepted key) are parsed, not the swapped file")
	assert.Equal(t, crSAKeyJSON(t, crPrincipalA), parsed)

	// Without a passed precondition there are no bytes, and the factory
	// never opens the path itself.
	_, err = act.newRuntime(context.Background(), inst)
	require.Error(t, err)
	assert.Equal(t, reasonCloudRunCredentialsFile, brokerhost.ReasonCode(err))
}

// TestFlatCloudRunCredentials_TypeRefused: authorized_user, external_account
// (including an executable credential source, which is never run),
// external_account_authorized_user, an unknown type, malformed JSON and an
// impersonated credential whose source is not a key are refused before the
// auth library acts; service_account and impersonated_service_account are
// accepted and build real credentials.
func TestFlatCloudRunCredentials_TypeRefused(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	script := filepath.Join(dir, "source.sh")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\ntouch "+marker+"\necho '{}'\n"), 0o700))
	t.Setenv("GOOGLE_EXTERNAL_ACCOUNT_ALLOW_EXECUTABLES", "1")

	refused := map[string]string{
		"authorized_user": `{"type":"authorized_user","client_id":"x","client_secret":"y","refresh_token":"z"}`,
		"external_account executable": `{"type":"external_account","audience":"//iam.googleapis.com/projects/1/locations/global/workloadIdentityPools/p/providers/q",` +
			`"subject_token_type":"urn:ietf:params:oauth:token-type:jwt","token_url":"https://sts.googleapis.com/v1/token",` +
			`"credential_source":{"executable":{"command":"` + script + `","timeout_millis":5000}}}`,
		"external_account_authorized_user": `{"type":"external_account_authorized_user","audience":"x","refresh_token":"y","token_url":"https://sts.googleapis.com/v1/oauthtoken"}`,
		"unknown type":                     `{"type":"gdch_service_account"}`,
		"no type":                          `{"client_email":"` + crPrincipalA + `"}`,
		"malformed":                        `{"type":`,
		"impersonated from authorized_user": string(crImpersonatedJSON(t, crPrincipalA,
			map[string]any{"type": "authorized_user", "client_id": "x", "client_secret": "y", "refresh_token": "z"})),
	}
	for name, body := range refused {
		t.Run(name, func(t *testing.T) {
			_, err := parseFlatCloudRunCredentials([]byte(body))
			require.Error(t, err)
			assert.Equal(t, reasonCloudRunCredentialsType, brokerhost.ReasonCode(err))
			assert.NotContains(t, err.Error(), "refresh_token")
		})
	}
	_, statErr := os.Stat(marker)
	assert.True(t, os.IsNotExist(statErr), "the executable credential source was never run")

	sa, err := parseFlatCloudRunCredentials(crSAKeyJSON(t, crPrincipalA))
	require.NoError(t, err)
	assert.Equal(t, flatCredentialInfo{credType: credentials.ServiceAccount, principal: crPrincipalA}, sa)
	creds, err := flatCloudRunCredentialsFromJSON(sa.credType, crSAKeyJSON(t, crPrincipalA))
	require.NoError(t, err)
	assert.NotNil(t, creds)

	imp := crImpersonatedJSON(t, crPrincipalB, crSAKey(t, crPrincipalA))
	info, err := parseFlatCloudRunCredentials(imp)
	require.NoError(t, err)
	assert.Equal(t, flatCredentialInfo{credType: credentials.ImpersonatedServiceAccount, principal: crPrincipalB, source: crPrincipalA}, info)
	creds, err = flatCloudRunCredentialsFromJSON(info.credType, imp)
	require.NoError(t, err)
	assert.NotNil(t, creds)
}

// TestFlatCloudRunPrincipal_Unresolvable: P8 refuses a key without a
// client_email and an impersonated credential whose target cannot be read.
func TestFlatCloudRunPrincipal_Unresolvable(t *testing.T) {
	for name, body := range map[string][]byte{
		"key without client_email": []byte(`{"type":"service_account","private_key":"x"}`),
		"impersonation url not IAM": func() []byte {
			b := crImpersonatedJSON(t, crPrincipalB, crSAKey(t, crPrincipalA))
			return []byte(strings.Replace(string(b), "iamcredentials.googleapis.com", "example.com", 1))
		}(),
		"impersonation url without target": func() []byte {
			b := crImpersonatedJSON(t, "", crSAKey(t, crPrincipalA))
			return b
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseFlatCloudRunCredentials(body)
			require.Error(t, err)
			assert.Equal(t, reasonCloudRunPrincipalUnresolved, brokerhost.ReasonCode(err))
		})
	}
}

// TestFlatCloudRunPrincipal_EqualsServiceAccount: P9 (CR3 step 2) refuses a
// configured service_account equal to the instance principal (a key of that
// account, or an impersonation targeting it: no self-impersonation), in any
// letter case, and equal to the account an impersonated credential starts
// from; a distinct account passes.
func TestFlatCloudRunPrincipal_EqualsServiceAccount(t *testing.T) {
	for name, body := range map[string][]byte{
		"key of the agent account":            crSAKeyJSON(t, crAgentSA),
		"key of the agent account, any case":  crSAKeyJSON(t, strings.ToUpper(crAgentSA)),
		"impersonating the agent account":     crImpersonatedJSON(t, crAgentSA, crSAKey(t, crPrincipalA)),
		"agent account impersonating another": crImpersonatedJSON(t, crPrincipalB, crSAKey(t, crAgentSA)),
		"agent account impersonating itself":  crImpersonatedJSON(t, crAgentSA, crSAKey(t, crAgentSA)),
	} {
		t.Run(name, func(t *testing.T) {
			info, err := parseFlatCloudRunCredentials(body)
			require.NoError(t, err)
			err = flatCheckPrincipalNotAgentSA(info, crAgentSA)
			require.Error(t, err)
			assert.Equal(t, reasonCloudRunPrincipalIsAgentSA, brokerhost.ReasonCode(err))
		})
	}
	info, err := parseFlatCloudRunCredentials(crImpersonatedJSON(t, crPrincipalB, crSAKey(t, crPrincipalA)))
	require.NoError(t, err)
	require.NoError(t, flatCheckPrincipalNotAgentSA(info, crAgentSA))

	// Through a host: refused before any cloud call; the sibling activates.
	dir := t.TempDir()
	r := newCRRig(t, crHostFacts())
	st := r.prepare(crDocker, crInst("cr-a", "us-central1", crWriteFile(t, dir, "cr.json", crSAKeyJSON(t, crAgentSA), 0o600)))
	assert.Equal(t, reasonCloudRunPrincipalIsAgentSA, st["cr-a"].Reason)
	assert.Zero(t, r.projectCalls)
	assert.Empty(t, r.built)
	assert.True(t, r.activated("docker-a"))
}

// TestFlatCloudRunPrincipal_SharedByTwoInstances: two instances with the
// same principal: the later one in configuration order is refused, the
// earlier one activates.
func TestFlatCloudRunPrincipal_SharedByTwoInstances(t *testing.T) {
	dir := t.TempDir()
	r := newCRRig(t, crHostFacts())
	st := r.prepare(
		crInst("cr-a", "us-central1", crWriteFile(t, dir, "a.json", crSAKeyJSON(t, crPrincipalA), 0o600)),
		crInst("cr-b", "europe-west4", crWriteFile(t, dir, "b.json", crImpersonatedJSON(t, strings.ToUpper(crPrincipalA), crSAKey(t, crPrincipalB)), 0o600)),
	)
	assert.True(t, r.activated("cr-a"), "the earlier instance activates")
	assert.Equal(t, brokerhost.StateRefused, st["cr-b"].State)
	assert.Equal(t, reasonCloudRunPrincipalShared, st["cr-b"].Reason)
	assert.Contains(t, st["cr-b"].Error, `instance "cr-a" already uses this credential principal`)
}

// TestFlatCloudRunPrincipal_EqualsHubADC: a determinable hub ADC principal
// equal to the instance principal refuses the instance; an undeterminable
// one skips the check, logged.
func TestFlatCloudRunPrincipal_EqualsHubADC(t *testing.T) {
	dir := t.TempDir()
	r := newCRRig(t, crHostFacts())
	r.hubID = strings.ToUpper(crPrincipalA)
	st := r.prepare(crInst("cr-a", "us-central1", crWriteFile(t, dir, "a.json", crSAKeyJSON(t, crPrincipalA), 0o600)))
	assert.Equal(t, reasonCloudRunPrincipalIsHub, st["cr-a"].Reason)
	assert.Zero(t, r.projectCalls, "refused before the probe")

	r = newCRRig(t, crHostFacts())
	st = r.prepare(crInst("cr-a", "us-central1", crWriteFile(t, dir, "b.json", crSAKeyJSON(t, crPrincipalA), 0o600)))
	assert.True(t, r.activated("cr-a"), "an undeterminable hub principal skips the check: %v", st["cr-a"].Error)
	require.Len(t, r.logs, 1)
	assert.Contains(t, r.logs[0], "cannot be determined")
}

// TestFlatCloudRunPrincipal_HubMetadataTimeoutSkipsOnly (amendment P5-CR, check
// P11): a metadata
// timeout in P11's lookup leaves the instance activating when every other
// check passes.
func TestFlatCloudRunPrincipal_HubMetadataTimeoutSkipsOnly(t *testing.T) {
	dir := t.TempDir()
	r := newCRRig(t, crHostFacts())
	r.hubErr = context.DeadlineExceeded
	st := r.prepare(crInst("cr-a", "us-central1", crWriteFile(t, dir, "a.json", crSAKeyJSON(t, crPrincipalA), 0o600)))
	assert.True(t, r.activated("cr-a"), "a metadata timeout skips only P11: %v", st["cr-a"].Error)
	assert.Equal(t, 1, r.hubCalls)
}

// TestFlatCloudRunPrincipal_ProbedComputeDefault: P13 refuses the probed
// project's default compute account, in any letter case.
func TestFlatCloudRunPrincipal_ProbedComputeDefault(t *testing.T) {
	for _, sa := range []string{crNumber + "-compute@developer.gserviceaccount.com", crNumber + "-COMPUTE@Developer.GServiceAccount.com"} {
		rt := crProbeRuntime(t, &resourcemanagerpb.Project{Name: "projects/" + crNumber, State: resourcemanagerpb.Project_ACTIVE}, nil, nil)
		inst := crInst("cr-a", "us-central1", "/etc/scion/cr.json")
		inst.RuntimeTarget.ServiceAccount = sa
		_, err := probeCloudRunExecutionScope(context.Background(), inst, rt)
		require.Error(t, err, sa)
		assert.Equal(t, reasonCloudRunDefaultSA, brokerhost.ReasonCode(err))
		assert.Equal(t, "the project default service accounts are not allowed", errors.Unwrap(err).Error())
	}
}

// crProbeRuntime is a fake flat runtime whose probe reads return
// proj/projErr and listErr.
func crProbeRuntime(t *testing.T, proj *resourcemanagerpb.Project, projErr, listErr error) *crFakeRuntime {
	t.Helper()
	return newCRFakeRuntime(config.CloudRunConfig{ProjectID: flatCRProject, Location: "us-central1"},
		func() (*resourcemanagerpb.Project, error) { return proj, projErr }, listErr)
}

func crProbe(t *testing.T, proj *resourcemanagerpb.Project, projErr, listErr error) error {
	t.Helper()
	_, err := probeCloudRunExecutionScope(context.Background(), crInst("cr-a", "us-central1", "/etc/scion/cr.json"), crProbeRuntime(t, proj, projErr, listErr))
	return err
}

var crActive = &resourcemanagerpb.Project{Name: "projects/" + crNumber, State: resourcemanagerpb.Project_ACTIVE}

// crProbeFailures are the probe failure classes and the diagnostic each
// carries.
var crProbeFailures = []struct {
	name    string
	proj    *resourcemanagerpb.Project
	projErr error
	listErr error
	want    string
}{
	{"Forbidden", nil, status.Error(codes.PermissionDenied, "denied"), nil, "access denied reading the configured project"},
	{"NotFound", nil, status.Error(codes.NotFound, "no"), nil, "the configured project was not found"},
	{"InactiveProject", &resourcemanagerpb.Project{Name: "projects/" + crNumber, State: resourcemanagerpb.Project_DELETE_REQUESTED}, nil, nil, "the configured project is not active"},
	{"EmptyNumber", &resourcemanagerpb.Project{Name: "projects/", State: resourcemanagerpb.Project_ACTIVE}, nil, nil, "has no project number"},
	{"RegionInvalid", crActive, nil, status.Error(codes.InvalidArgument, "bad region"), "the configured region is not valid"},
	{"ListForbidden", crActive, nil, status.Error(codes.PermissionDenied, "denied"), "access denied listing Cloud Run instances"},
	{"Transport", nil, status.Error(codes.Unavailable, "down"), nil, "Resource Manager unreachable"},
	{"ListTransport", crActive, nil, errors.New("connection reset"), "Cloud Run unreachable"},
}

func crRunProbeFailure(t *testing.T, name string) {
	t.Helper()
	for _, tc := range crProbeFailures {
		if tc.name != name {
			continue
		}
		err := crProbe(t, tc.proj, tc.projErr, tc.listErr)
		require.Error(t, err)
		assert.ErrorIs(t, err, brokeridentity.ErrExecutionScopeUnidentified)
		assert.Contains(t, err.Error(), tc.want)
		assert.NotContains(t, err.Error(), "/etc/scion", "no credentials path")
		assert.NotContains(t, err.Error(), crNumber, "no project number")
		return
	}
	t.Fatalf("no probe case %q", name)
}

func TestProbeCloudRunScope_Forbidden(t *testing.T)       { crRunProbeFailure(t, "Forbidden") }
func TestProbeCloudRunScope_NotFound(t *testing.T)        { crRunProbeFailure(t, "NotFound") }
func TestProbeCloudRunScope_InactiveProject(t *testing.T) { crRunProbeFailure(t, "InactiveProject") }
func TestProbeCloudRunScope_RegionInvalid(t *testing.T)   { crRunProbeFailure(t, "RegionInvalid") }
func TestProbeCloudRunScope_ListForbidden(t *testing.T)   { crRunProbeFailure(t, "ListForbidden") }
func TestProbeCloudRunScope_Transport(t *testing.T) {
	crRunProbeFailure(t, "Transport")
	crRunProbeFailure(t, "ListTransport")
	crRunProbeFailure(t, "EmptyNumber")
}

// TestProbeCloudRunScope_BindsProbedNumber: a successful probe returns the
// cloudrun scope and binds the probed number into the runtime, while the
// runtime keeps the configured project ID.
func TestProbeCloudRunScope_BindsProbedNumber(t *testing.T) {
	rt := crProbeRuntime(t, crActive, nil, nil)
	scope, err := probeCloudRunExecutionScope(context.Background(), crInst("cr-a", "us-central1", "/etc/scion/cr.json"), rt)
	require.NoError(t, err)
	assert.Equal(t, brokeridentity.ExecutionScope{Type: brokeridentity.TargetTypeCloudRun,
		CloudRun: &brokeridentity.CloudRunScope{ProjectNumber: crNumber, Region: "us-central1", ProjectID: flatCRProject}}, scope)
	assert.Equal(t, flatCRProject, rt.ProjectID())
	assert.Equal(t, crNumber, rt.bound, "the probed number is bound into the runtime")
	assert.ErrorIs(t, rt.BindProjectNumber("999"), runtime.ErrProjectNumberBound, "the number is bound once")
}

// TestFlatHost_CloudRunProbeFailureSiblingActivates: for each probe failure
// class the cloudrun instance is refused and a docker sibling activates.
func TestFlatHost_CloudRunProbeFailureSiblingActivates(t *testing.T) {
	for _, tc := range crProbeFailures {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			r := newCRRig(t, crHostFacts())
			r.project, r.projectErr, r.listErr = tc.proj, tc.projErr, tc.listErr
			st := r.prepare(crDocker, crInst("cr-a", "us-central1", crWriteFile(t, dir, "a.json", crSAKeyJSON(t, crPrincipalA), 0o600)))
			assert.Equal(t, brokerhost.StateRefused, st["cr-a"].State)
			assert.Equal(t, "scope_unidentified", st["cr-a"].Reason)
			assert.False(t, r.activated("cr-a"))
			assert.True(t, r.activated("docker-a"))
		})
	}
}

// TestFlatCloudRunCredentials_ExplicitFileNoADCFallback: with a malformed or
// unsupported credentials file and usable ADC available, the instance is
// refused: no credential is built, no hub principal lookup or cloud call is
// sent, and a sibling activates.
func TestFlatCloudRunCredentials_ExplicitFileNoADCFallback(t *testing.T) {
	dir := t.TempDir()
	adc := crWriteFile(t, dir, "adc.json", crSAKeyJSON(t, crPrincipalB), 0o600)
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", adc)
	for name, body := range map[string][]byte{
		"malformed":       []byte("not json"),
		"authorized_user": []byte(`{"type":"authorized_user","client_id":"x","client_secret":"y","refresh_token":"z"}`),
	} {
		t.Run(name, func(t *testing.T) {
			r := newCRRig(t, crHostFacts())
			st := r.prepare(crDocker, crInst("cr-a", "us-central1", crWriteFile(t, t.TempDir(), "cr.json", body, 0o600)))
			assert.Equal(t, reasonCloudRunCredentialsType, st["cr-a"].Reason)
			assert.Empty(t, r.built, "no credential built, from the file or from ADC")
			assert.Zero(t, r.hubCalls)
			assert.Zero(t, r.projectCalls)
			assert.True(t, r.activated("docker-a"))
			assert.Equal(t, adc, os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"), "the process environment keeps its value")
		})
	}
}

// TestFlatCloudRunCredentials_PerInstanceIsolation: two instances with two
// fixture files get two distinct credential objects, each from its own
// file, and the process environment keeps its values.
func TestFlatCloudRunCredentials_PerInstanceIsolation(t *testing.T) {
	dir := t.TempDir()
	before := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")
	r := newCRRig(t, crHostFacts())
	r.prepare(
		crInst("cr-a", "us-central1", crWriteFile(t, dir, "a.json", crSAKeyJSON(t, crPrincipalA), 0o600)),
		crInst("cr-b", "europe-west4", crWriteFile(t, dir, "b.json", crSAKeyJSON(t, crPrincipalB), 0o600)),
	)
	assert.True(t, r.activated("cr-a"))
	assert.True(t, r.activated("cr-b"))
	require.Len(t, r.built, 2)
	assert.NotSame(t, r.built[0], r.built[1], "each instance has its own credentials")
	assert.Equal(t, before, os.Getenv("GOOGLE_APPLICATION_CREDENTIALS"))
}

// TestFlatCloudRun_LogsConfiguredServiceAccount: each active cloudrun
// instance's configured service_account is logged locally; other types are
// not.
func TestFlatCloudRun_LogsConfiguredServiceAccount(t *testing.T) {
	var logs []string
	logFlatCloudRunServiceAccounts(func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }, []brokerhost.ActiveInstance{
		{Context: brokerhost.InstanceContext{Instance: crDocker}},
		{Context: brokerhost.InstanceContext{Instance: crInst("cr-a", "us-central1", "/etc/scion/cr.json")}},
	})
	require.Len(t, logs, 1)
	assert.Equal(t, `Flat Runtime Broker instance "cr-a": new Cloud Run agents run as service account `+crAgentSA, logs[0])
	assert.NotContains(t, logs[0], "/etc/scion")
}

// TestRegisterInstance_CloudRunRefusedByPrecheck: 'register --instance' for
// a cloudrun instance is refused by the shared precheck (P1: this is the
// remote registration flow): no identity directory and no Hub call.
func TestRegisterInstance_CloudRunRefusedByPrecheck(t *testing.T) {
	globalDir := setupRegisterInstanceTest(t, "https://hub.example:8080")
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(
		"schema_version: \"1\"\nserver:\n  broker:\n    enabled: true\n    instances:\n      - key: cr-a\n        name: cr-a\n        runtime_target:\n"+
			"          type: cloudrun\n          project_id: example-project\n          region: us-central1\n"+
			"          service_account: "+crAgentSA+"\n          subnetwork: agents-subnet\n          credentials_file: /etc/scion/cr-a.json\n"), 0o644))
	crExpectRegisterRefused(t, globalDir, "cr-a", "cloudrun targets require a colocated host")
}

// TestRegisterInstance_DockerRefusedByDefaultVolumeCheck: a docker instance
// the host would refuse for its default volumes mints nothing.
func TestRegisterInstance_DockerRefusedByDefaultVolumeCheck(t *testing.T) {
	globalDir := setupRegisterInstanceTest(t, "https://hub.example:8080")
	control := filepath.Join(os.Getenv("HOME"), ".scion")
	require.NoError(t, os.MkdirAll(control, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(
		"schema_version: \"1\"\nserver:\n  broker:\n    enabled: true\n    instances:\n      - key: remote-docker\n        name: example-remote\n        runtime_target:\n"+
			"          type: docker\n        agent_defaults:\n          volumes:\n            - source: "+control+"\n              target: /x\n              read_only: true\n"), 0o644))
	crExpectRegisterRefused(t, globalDir, "remote-docker", "agent_defaults.volumes[0]")
}

func crExpectRegisterRefused(t *testing.T, globalDir, key, want string) {
	t.Helper()
	brokerRegisterInstance = key
	orig, origProber := registerFlatInstance, flatScopeProber
	t.Cleanup(func() { registerFlatInstance, flatScopeProber = orig, origProber })
	called, probed := false, false
	registerFlatInstance = func(context.Context, hubclient.Client, config.V1RuntimeBrokerInstanceConfig, *brokeridentity.Identity,
		string, string, brokerregistration.Options) (*brokercredentials.BrokerCredentials, error) {
		called = true
		return nil, errors.New("must not register")
	}
	flatScopeProber = func(context.Context, config.V1RuntimeBrokerInstanceConfig, runtime.Runtime) (brokeridentity.ExecutionScope, error) {
		probed = true
		return brokeridentity.ExecutionScope{}, errors.New("must not probe")
	}
	err := runBrokerRegister(brokerRegisterCmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), want)
	assert.Contains(t, err.Error(), "not registered")
	assert.False(t, called, "no Hub call")
	assert.False(t, probed, "no scope probe")
	_, statErr := os.Stat(brokeridentity.InstanceDir(globalDir, key))
	assert.True(t, os.IsNotExist(statErr), "no identity directory is created")
}

// crActivatedIdentity activates one cloudrun instance whose credentials file
// is a key of crPrincipalA and returns the bytes of its identity file, its
// health JSON and the instance.
func crActivatedIdentity(t *testing.T) (identity, health []byte, inst config.V1RuntimeBrokerInstanceConfig, key []byte) {
	t.Helper()
	dir := t.TempDir()
	key = crSAKeyJSON(t, crPrincipalA)
	inst = crInst("cr-a", "us-central1", crWriteFile(t, dir, "a.json", key, 0o600))
	inst.RuntimeTarget.Network = "agents-net"
	r := newCRRig(t, crHostFacts())
	globalDir := t.TempDir()
	h, err := brokerhost.New(brokerhost.Config{
		GlobalDir: globalDir, Instances: []config.V1RuntimeBrokerInstanceConfig{inst}, Mode: brokerhost.ModeColocated,
		Precondition: r.act.precondition, NewRuntime: r.act.newRuntime, ProbeScope: probeFlatInstanceScope, Activator: r.rec,
		BuildServer: func(brokerhost.InstanceContext) (*runtimebroker.Server, error) {
			return nil, errors.New("not serving in this test")
		},
	})
	require.NoError(t, err)
	require.NoError(t, h.Prepare(context.Background()))
	require.True(t, r.activated("cr-a"))
	identity, err = os.ReadFile(filepath.Join(brokeridentity.InstanceDir(globalDir, "cr-a"), brokeridentity.IdentityFileName))
	require.NoError(t, err)
	health, err = json.Marshal(h.Status())
	require.NoError(t, err)
	return identity, health, inst, key
}

// TestFlatCloudRun_NoLocalDetailReachesHub: what leaves the host for a flat cloudrun
// instance carries none of the service account, network, subnetwork or
// credentials path: the runtime target descriptor has only id, type and
// display name; the presentation (registration, heartbeat) and the health
// JSON name none of them, nor the project ID; the identity file holds the
// project ID informationally and none of the others.
func TestFlatCloudRun_NoLocalDetailReachesHub(t *testing.T) {
	identity, health, inst, _ := crActivatedIdentity(t)
	tg := inst.RuntimeTarget
	secretish := []string{tg.ServiceAccount, tg.Network, tg.Subnetwork, tg.CredentialsFile, filepath.Dir(tg.CredentialsFile), crPrincipalA}

	assert.Equal(t, []string{"ID", "Type", "DisplayName"}, reflectFields(api.RuntimeTargetDescriptor{}),
		"the descriptor carries only id, type and display name")

	pres, err := json.Marshal(config.PresentInstance(inst).API())
	require.NoError(t, err)
	for _, s := range append(secretish, tg.ProjectID, crNumber) {
		assert.NotContains(t, string(pres), s, "presentation")
		assert.NotContains(t, string(health), s, "health")
	}
	for _, s := range secretish {
		assert.NotContains(t, string(identity), s, "identity file")
	}
	assert.Contains(t, string(identity), crNumber, "the identity file holds the probed number")
}

// TestFlatCloudRun_CredentialContentsNeverInAPI: no body the host produces
// for a flat cloudrun instance (presentation, health, identity) contains the
// credentials file's contents.
func TestFlatCloudRun_CredentialContentsNeverInAPI(t *testing.T) {
	identity, health, inst, key := crActivatedIdentity(t)
	pres, err := json.Marshal(config.PresentInstance(inst).API())
	require.NoError(t, err)
	var parsed map[string]string
	require.NoError(t, json.Unmarshal(key, &parsed))
	pemBody := strings.Split(parsed["private_key"], "\n")[1]
	for name, body := range map[string][]byte{"presentation": pres, "health": health, "identity": identity} {
		assert.NotContains(t, string(body), pemBody, name)
		assert.NotContains(t, string(body), parsed["private_key_id"], name)
		assert.NotContains(t, string(body), parsed["client_id"], name)
	}
}

// reflectFields lists a struct's field names in order.
func reflectFields(v any) []string {
	t := reflect.TypeOf(v)
	out := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		out = append(out, t.Field(i).Name)
	}
	return out
}

// crJSON marshals m.
func crJSON(t *testing.T, m map[string]any) []byte {
	t.Helper()
	b, err := json.Marshal(m)
	require.NoError(t, err)
	return b
}

// crImpersonated is an impersonated credential of target from source, with
// extra top-level fields.
func crImpersonated(target string, source map[string]any, extra map[string]any) map[string]any {
	m := map[string]any{
		"type":                              "impersonated_service_account",
		"service_account_impersonation_url": "https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/" + target + ":generateAccessToken",
		"source_credentials":                source,
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

// TestFlatCloudRunCredentials_DelegatesAndEndpointsRefused (errata E-6, E-7):
// an impersonated credential with a delegates chain, a key or source key
// with another token_uri, and a non-default universe_domain (top level or
// in the source) are refused with cloudrun_credentials_type, naming no file
// content; an empty or absent delegates list, and a key with neither
// token_uri nor universe_domain (or the default ones), are accepted.
func TestFlatCloudRunCredentials_DelegatesAndEndpointsRefused(t *testing.T) {
	const otherURI = "https://token.example.net/token"
	const delegate = "hop-1@example-project.iam.gserviceaccount.com"
	keyWith := func(k string, v any) map[string]any {
		m := crSAKey(t, crPrincipalA)
		if v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
		return m
	}
	refused := map[string][]byte{
		"impersonated with one delegate":    crJSON(t, crImpersonated(crPrincipalB, crSAKey(t, crPrincipalA), map[string]any{"delegates": []string{delegate}})),
		"impersonated with malformed chain": crJSON(t, crImpersonated(crPrincipalB, crSAKey(t, crPrincipalA), map[string]any{"delegates": delegate})),
		"key with another token_uri":        crJSON(t, keyWith("token_uri", otherURI)),
		"key with another universe":         crJSON(t, keyWith("universe_domain", "example.net")),
		"source key with another token_uri": crJSON(t, crImpersonated(crPrincipalB, keyWith("token_uri", otherURI), nil)),
		"source key with another universe":  crJSON(t, crImpersonated(crPrincipalB, keyWith("universe_domain", "example.net"), nil)),
		"impersonated with another universe": crJSON(t, crImpersonated(crPrincipalB, crSAKey(t, crPrincipalA),
			map[string]any{"universe_domain": "example.net"})),
	}
	for name, body := range refused {
		t.Run(name, func(t *testing.T) {
			_, err := parseFlatCloudRunCredentials(body)
			require.Error(t, err)
			assert.Equal(t, reasonCloudRunCredentialsType, brokerhost.ReasonCode(err))
			for _, content := range []string{delegate, otherURI, "example.net", crPrincipalA, crPrincipalB} {
				assert.NotContains(t, err.Error(), content, "the refusal names no file content")
			}
		})
	}
	accepted := map[string][]byte{
		"impersonated with empty delegates":  crJSON(t, crImpersonated(crPrincipalB, crSAKey(t, crPrincipalA), map[string]any{"delegates": []string{}})),
		"impersonated with null delegates":   crJSON(t, crImpersonated(crPrincipalB, crSAKey(t, crPrincipalA), map[string]any{"delegates": nil})),
		"impersonated without delegates":     crJSON(t, crImpersonated(crPrincipalB, crSAKey(t, crPrincipalA), nil)),
		"key with neither field":             crJSON(t, keyWith("token_uri", nil)),
		"key with the default universe":      crJSON(t, keyWith("universe_domain", "googleapis.com")),
		"source key without token_uri":       crJSON(t, crImpersonated(crPrincipalB, keyWith("token_uri", nil), nil)),
		"impersonated with default universe": crJSON(t, crImpersonated(crPrincipalB, crSAKey(t, crPrincipalA), map[string]any{"universe_domain": "googleapis.com"})),
	}
	for name, body := range accepted {
		t.Run(name, func(t *testing.T) {
			_, err := parseFlatCloudRunCredentials(body)
			require.NoError(t, err)
		})
	}
}

// TestFlatOwnershipPreflight_CloudRunDrainListCapped: with more than 10
// unlabeled agent instances on the scope, the refusal keeps the exact count
// and the drain wording, lists the first 10 instance names (sorted), then
// "and M more".
func TestFlatOwnershipPreflight_CloudRunDrainListCapped(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	const mine = "6c0f9a3e-8d1b-4e2f-a5c7-3b9d0e1f2a4c"
	fake := &flatCRInstances{}
	var want []string
	for i := 11; i >= 0; i-- { // listed out of order: the refusal sorts
		id := fmt.Sprintf("legacy-%02d", i)
		fake.instances = append(fake.instances, flatCRInstance("us-central1", id, "", fmt.Sprintf("aa2b3c4d-5e6f-4071-8293-a4b5c6d7e8%02d", i), id, fmt.Sprintf("31436587-a9cb-4fed-8102-1324354657%02d", i)))
	}
	for i := 0; i < 10; i++ {
		want = append(want, fmt.Sprintf("legacy-%02d", i))
	}
	err := flatOwnershipPreflight(context.Background(), brokerhost.Candidate{
		Instance: config.V1RuntimeBrokerInstanceConfig{Key: "cr-a", Name: "a", RuntimeTarget: flatCRTarget("us-central1")},
		Identity: &brokeridentity.Identity{InstanceKey: "cr-a", RuntimeBrokerID: mine},
		Runtime:  flatCRRuntime(t, "us-central1", fake),
	})
	require.Error(t, err)
	assert.Equal(t, "12 Cloud Run agent instance(s) in this project and region carry no owner label "+
		"(for example, created by a legacy Cloud Run Runtime Broker); drain them through the Runtime Broker that created them, then restart (instances: "+
		strings.Join(want, ", ")+", and 2 more)", err.Error())
	assert.NotContains(t, err.Error(), "legacy-10")
	assert.NotContains(t, err.Error(), "legacy-11")

	// The function sorts its own copy: unsorted input gives the same text,
	// and the caller's slice is not reordered.
	unsorted := []string{"legacy-11", "legacy-03", "legacy-00", "legacy-10", "legacy-07", "legacy-01", "legacy-09", "legacy-05", "legacy-02", "legacy-08", "legacy-04", "legacy-06"}
	before := slices.Clone(unsorted)
	assert.Equal(t, err.Error(), flatCloudRunUnlabeledError(unsorted).Error())
	assert.Equal(t, before, unsorted, "the caller's slice is left as it was")

	// Exactly 10: all listed, no tail.
	assert.Equal(t, "10 Cloud Run agent instance(s) in this project and region carry no owner label "+
		"(for example, created by a legacy Cloud Run Runtime Broker); drain them through the Runtime Broker that created them, then restart (instances: "+
		strings.Join(want, ", ")+")", flatCloudRunUnlabeledError(want).Error())
}

// TestProbeFlatInstanceScope_CloudRunOnlyForCloudRunTarget: the prober
// treats a runtime as a Cloud Run scope only for an instance whose target
// type is cloudrun; any other instance carrying such a runtime is
// unidentified, and its runtime is never asked to probe.
func TestProbeFlatInstanceScope_CloudRunOnlyForCloudRunTarget(t *testing.T) {
	for _, inst := range []config.V1RuntimeBrokerInstanceConfig{
		crDocker,
		{Key: "k8s-a", Name: "k", RuntimeTarget: &config.V1RuntimeTargetConfig{Type: config.RuntimeTargetTypeKubernetes}},
		{Key: "none", Name: "n"},
	} {
		rt := crProbeRuntime(t, crActive, nil, nil)
		_, err := probeFlatInstanceScope(context.Background(), inst, rt)
		require.Error(t, err, inst.Key)
		assert.ErrorIs(t, err, brokeridentity.ErrExecutionScopeUnidentified, inst.Key)
		assert.Zero(t, rt.probeCalls, "%s: the runtime is never asked to probe", inst.Key)
		assert.Empty(t, rt.bound, "%s: nothing is bound", inst.Key)
	}
	// Control: the same runtime on a cloudrun instance is probed.
	rt := crProbeRuntime(t, crActive, nil, nil)
	scope, err := probeFlatInstanceScope(context.Background(), crInst("cr-a", "us-central1", "/etc/scion/cr.json"), rt)
	require.NoError(t, err)
	assert.Equal(t, brokeridentity.TargetTypeCloudRun, scope.Type)
	assert.Equal(t, 1, rt.probeCalls)
}
