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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/auth"
	"cloud.google.com/go/auth/credentials"
	"cloud.google.com/go/compute/metadata"
	resourcemanagerpb "cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/GoogleCloudPlatform/scion/pkg/brokerhost"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/projectfs"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
)

// Flat cloudrun instance activation (amendment P5-CR, CR3 activation order).
// Every path that builds a cloudrun runtime or mints its identity (the
// host's Prepare and 'scion broker register --instance') goes through one
// flatInstanceActivation:
//
//  1. precondition (the host's Precondition hook, before the runtime is
//     built; no cloud or metadata call): flatInstancePrecheck, P1-P6. For
//     every target type it also applies the per-instance default-volume
//     check the host applies at start.
//  2. newRuntime (the runtime factory): for cloudrun, the credential checks
//     P7-P11 on the bytes P6 read from the checked handle, then the
//     credentials are built once and the runtime is constructed with them.
//  3. the scope probe (probeCloudRunExecutionScope, P12), P13, and the
//     binding of the probed project number into the runtime.
//
// Refusals carry the health reason codes below. No message carries the
// credentials-file path, credential contents or token material.

// Health reason codes of a refused cloudrun instance (amendment P5-CR, CR3
// activation order, checks P1-P13; listed, not frozen).
const (
	reasonCloudRunRemoteUnsupported   = "cloudrun_remote_unsupported"
	reasonCloudRunHostOnCloudRun      = "cloudrun_host_on_cloudrun"
	reasonCloudRunStorageBackend      = "cloudrun_storage_backend"
	reasonCloudRunHomesPlacement      = "cloudrun_homes_placement"
	reasonCloudRunHubEndpoint         = "cloudrun_hub_endpoint"
	reasonCloudRunCredentialsFile     = "cloudrun_credentials_file"
	reasonCloudRunCredentialsType     = "cloudrun_credentials_type"
	reasonCloudRunPrincipalUnresolved = "cloudrun_principal_unresolved"
	reasonCloudRunPrincipalIsAgentSA  = "cloudrun_principal_is_agent_sa"
	reasonCloudRunPrincipalShared     = "cloudrun_principal_shared"
	reasonCloudRunPrincipalIsHub      = "cloudrun_principal_is_hub"
	reasonCloudRunDefaultSA           = "cloudrun_default_sa"
)

// flatCredentialsMaxBytes bounds a credentials file read (a key file is a
// few KiB).
const flatCredentialsMaxBytes = 64 << 10

// flatCloudRunProbeTimeout bounds each read of the scope probe.
const flatCloudRunProbeTimeout = 30 * time.Second

// flatHubMetadataTimeout bounds P11's read of the hub host's own metadata
// server.
const flatHubMetadataTimeout = 3 * time.Second

// flatCloudRunScopes are the OAuth scopes of a flat instance's credentials.
var flatCloudRunScopes = []string{"https://www.googleapis.com/auth/cloud-platform"}

// flatPrecheckHost is what the shared precheck needs from the process.
type flatPrecheckHost struct {
	// mode is the host's activation mode; 'register --instance' is the
	// remote registration flow (ModeRemote).
	mode brokerhost.Mode
	// layout and storageErr are the host's project storage layout
	// (brokerStorageLayout) and the error that left it unusable.
	layout     projectfs.Layout
	storageErr error
	// hubEndpoint and hubEndpointSrc are resolveHubEndpointWithSource's
	// result: the input of the agent-facing hub URL.
	hubEndpoint    string
	hubEndpointSrc hubEndpointSource
	// getenv reads one variable by its exact name (nil: os.Getenv).
	getenv func(string) string
}

// flatCheckedCredentials are the credential bytes P6 read from the handle
// that passed the file checks (amendment P5-CR, CR3 credentials; checks P6
// and P7): the factory parses these and never opens
// the path again.
type flatCheckedCredentials struct {
	data []byte
}

// flatInstancePrecheck runs the local activation checks of one instance,
// before its runtime is built (P1-P6 for cloudrun), with no cloud or
// metadata call. For every target type it applies the per-instance
// default-volume check the host applies at start, so 'register --instance'
// mints nothing for a configuration the host would refuse. It returns the
// checked credential bytes of a cloudrun instance (nil for other types).
func flatInstancePrecheck(_ context.Context, inst config.V1RuntimeBrokerInstanceConfig, hc flatPrecheckHost) (*flatCheckedCredentials, error) {
	if inst.RuntimeTarget == nil {
		return nil, errors.New("runtime_target is required")
	}
	if err := brokerhost.CheckInstanceDefaultVolumesAt(fmt.Sprintf("server.broker.instances[%q]", inst.Key), inst); err != nil {
		return nil, err
	}
	t := inst.RuntimeTarget
	if t.Type != config.RuntimeTargetTypeCloudRun {
		return nil, nil
	}
	// P1
	if hc.mode != brokerhost.ModeColocated {
		return nil, brokerhost.WithReason(reasonCloudRunRemoteUnsupported,
			errors.New("cloudrun targets require a colocated host in this release"))
	}
	// P2: K_SERVICE, read by its exact name.
	getenv := hc.getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	if getenv("K_SERVICE") != "" {
		return nil, brokerhost.WithReason(reasonCloudRunHostOnCloudRun,
			errors.New("a flat host on a Cloud Run service has no durable identity; run the host on a VM or GKE, or keep the legacy broker"))
	}
	// P3, P4
	if hc.storageErr != nil || hc.layout.Backend != projectfs.BackendNFS {
		return nil, brokerhost.WithReason(reasonCloudRunStorageBackend,
			errors.New("the nfs workspace backend (server.project_storage) is required for Cloud Run agents"))
	}
	if hc.layout.Homes != projectfs.BackendNFS {
		return nil, brokerhost.WithReason(reasonCloudRunHomesPlacement,
			errors.New("agent homes on Cloud Run are always on the share; set project_storage.homes: nfs"))
	}
	// P5: the same function that produces the value applied to the
	// instance (flatCloudRunContainerHub).
	if _, err := flatCloudRunAgentHubEndpoint(hc.hubEndpoint, hc.hubEndpointSrc); err != nil {
		return nil, brokerhost.WithReason(reasonCloudRunHubEndpoint,
			fmt.Errorf("no https, non-loopback hub endpoint for Cloud Run agents: %w", err))
	}
	// P6: checked on the opened handle; the bytes come from that handle.
	data, err := openFlatCredentialsFile(t.CredentialsFile)
	if err != nil {
		return nil, brokerhost.WithReason(reasonCloudRunCredentialsFile, err)
	}
	return &flatCheckedCredentials{data: data}, nil
}

// flatCloudRunAgentHubEndpoint is the one source of the hub URL a flat
// cloudrun instance gives its agents: the hub's public endpoint, which must
// pass runtimebroker.ValidateFlatCloudRunHubEndpoint and must not be derived
// from the IAP audience (agents hold no IAP credential). P5 checks it and
// flatCloudRunContainerHub applies it, so the two cannot drift. Nothing in
// it comes from agent-controlled data or agent-instance metadata.
func flatCloudRunAgentHubEndpoint(publicEndpoint string, src hubEndpointSource) (string, error) {
	if src == hubEndpointSourceIAPAudience {
		return "", errors.New("the hub's public endpoint is derived from the IAP audience, which Cloud Run agents cannot pass")
	}
	ep := strings.TrimRight(publicEndpoint, "/")
	if err := runtimebroker.ValidateFlatCloudRunHubEndpoint(ep); err != nil {
		return "", err
	}
	return ep, nil
}

// flatCloudRunContainerHub is the container Hub setting of a flat cloudrun
// instance: the P5-validated endpoint, or none (the instance is refused at
// activation, and the broker refuses a start without a valid one).
func flatCloudRunContainerHub(publicEndpoint string, src hubEndpointSource) containerHubEndpointResult {
	ep, err := flatCloudRunAgentHubEndpoint(publicEndpoint, src)
	if err != nil {
		return containerHubEndpointResult{}
	}
	return containerHubEndpointResult{Endpoint: ep}
}

// flatCredentialInfo is what the credential checks read from a credentials
// file, before the auth library sees it.
type flatCredentialInfo struct {
	// credType is the accepted type: service_account or
	// impersonated_service_account.
	credType credentials.CredType
	// principal is the instance principal: client_email for a key, the
	// impersonation target for an impersonated credential.
	principal string
	// source is the client_email of an impersonated credential's source
	// key ("" for a key file).
	source string
}

// flatImpersonationURL matches the IAM Credentials generateAccessToken URL
// of an impersonated credential and captures its target account.
var flatImpersonationURL = regexp.MustCompile(`^https://iamcredentials\.googleapis\.com/v1/projects/-/serviceAccounts/([^/:]+@[^/:]+):generateAccessToken$`)

// parseFlatCloudRunCredentials applies the type allow-list (P7) and resolves
// the instance principal (P8), before the auth library acts on the file.
// Only service_account keys and impersonated_service_account credentials
// whose source is a service_account key are accepted; every other type
// (authorized_user, external_account with any credential_source,
// external_account_authorized_user, ...) is refused. Messages carry no
// file contents beyond the (truncated) type name.
func parseFlatCloudRunCredentials(data []byte) (flatCredentialInfo, error) {
	var f struct {
		Type                           string          `json:"type"`
		ClientEmail                    string          `json:"client_email"`
		TokenURI                       string          `json:"token_uri"`
		UniverseDomain                 string          `json:"universe_domain"`
		ServiceAccountImpersonationURL string          `json:"service_account_impersonation_url"`
		SourceCredentials              json.RawMessage `json:"source_credentials"`
		Delegates                      json.RawMessage `json:"delegates"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return flatCredentialInfo{}, brokerhost.WithReason(reasonCloudRunCredentialsType,
			errors.New("credentials_file is not a JSON credential file"))
	}
	if err := flatCheckUniverse(f.UniverseDomain); err != nil {
		return flatCredentialInfo{}, err
	}
	switch f.Type {
	case string(credentials.ServiceAccount):
		if err := flatCheckTokenURI(f.TokenURI); err != nil {
			return flatCredentialInfo{}, err
		}
		if !flatLooksLikeEmail(f.ClientEmail) {
			return flatCredentialInfo{}, brokerhost.WithReason(reasonCloudRunPrincipalUnresolved,
				errors.New("cannot determine the instance credential principal (the key file has no client_email)"))
		}
		return flatCredentialInfo{credType: credentials.ServiceAccount, principal: f.ClientEmail}, nil
	case string(credentials.ImpersonatedServiceAccount):
		// A delegation chain is refused: each delegate can mint a token for
		// the next hop, so an account in the chain (the agent's service
		// account, say) could act as the instance principal, and P9 and
		// P11 see only the source and the target.
		if flatNonEmptyJSONArray(f.Delegates) {
			return flatCredentialInfo{}, brokerhost.WithReason(reasonCloudRunCredentialsType,
				errors.New("an impersonated credential with delegates is not accepted for Cloud Run instances"))
		}
		var src struct {
			Type           string `json:"type"`
			ClientEmail    string `json:"client_email"`
			TokenURI       string `json:"token_uri"`
			UniverseDomain string `json:"universe_domain"`
		}
		if len(f.SourceCredentials) == 0 || json.Unmarshal(f.SourceCredentials, &src) != nil {
			return flatCredentialInfo{}, brokerhost.WithReason(reasonCloudRunCredentialsType,
				errors.New("the impersonated credential has no readable source credential"))
		}
		if src.Type != string(credentials.ServiceAccount) {
			return flatCredentialInfo{}, brokerhost.WithReason(reasonCloudRunCredentialsType,
				fmt.Errorf("the impersonated credential's source type %q is not accepted for Cloud Run instances (only service_account)", flatTruncate(src.Type)))
		}
		if err := flatCheckUniverse(src.UniverseDomain); err != nil {
			return flatCredentialInfo{}, err
		}
		if err := flatCheckTokenURI(src.TokenURI); err != nil {
			return flatCredentialInfo{}, err
		}
		m := flatImpersonationURL.FindStringSubmatch(f.ServiceAccountImpersonationURL)
		if m == nil {
			return flatCredentialInfo{}, brokerhost.WithReason(reasonCloudRunPrincipalUnresolved,
				errors.New("cannot determine the instance credential principal (the impersonation target is unreadable)"))
		}
		if !flatLooksLikeEmail(src.ClientEmail) {
			return flatCredentialInfo{}, brokerhost.WithReason(reasonCloudRunPrincipalUnresolved,
				errors.New("cannot determine the account the impersonated credential starts from"))
		}
		return flatCredentialInfo{credType: credentials.ImpersonatedServiceAccount, principal: m[1], source: src.ClientEmail}, nil
	default:
		return flatCredentialInfo{}, brokerhost.WithReason(reasonCloudRunCredentialsType,
			fmt.Errorf("credential type %q is not accepted for Cloud Run instances", flatTruncate(f.Type)))
	}
}

// flatCheckPrincipalNotAgentSA is P9: the configured service_account must
// differ from the instance principal and, for an impersonated credential,
// from the account the impersonation starts from (either way the agent,
// which holds its service account's token, could act as the instance).
func flatCheckPrincipalNotAgentSA(info flatCredentialInfo, serviceAccount string) error {
	if strings.EqualFold(info.principal, serviceAccount) {
		return brokerhost.WithReason(reasonCloudRunPrincipalIsAgentSA,
			errors.New("service_account must differ from the instance credential principal"))
	}
	if info.source != "" && strings.EqualFold(info.source, serviceAccount) {
		return brokerhost.WithReason(reasonCloudRunPrincipalIsAgentSA,
			errors.New("service_account must differ from the account the instance credential impersonates from"))
	}
	return nil
}

// flatTokenURI is the only token endpoint a flat cloudrun service account
// key (or an impersonated credential's source key) may name.
const flatTokenURI = "https://oauth2.googleapis.com/token"

// flatCheckTokenURI refuses a key whose token_uri is set to anything other
// than the Google OAuth token endpoint (an absent one defaults to it).
func flatCheckTokenURI(uri string) error {
	if uri != "" && uri != flatTokenURI {
		return brokerhost.WithReason(reasonCloudRunCredentialsType,
			errors.New("the credential's token_uri must be absent or "+flatTokenURI))
	}
	return nil
}

// flatCheckUniverse refuses a credential for a universe domain other than
// the default (googleapis.com, or none named).
func flatCheckUniverse(domain string) error {
	if domain != "" && domain != "googleapis.com" {
		return brokerhost.WithReason(reasonCloudRunCredentialsType,
			errors.New("the credential's universe_domain must be googleapis.com"))
	}
	return nil
}

// flatNonEmptyJSONArray reports whether raw is anything other than absent,
// null or an empty JSON array (a malformed value counts as non-empty, so it
// is refused).
func flatNonEmptyJSONArray(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var a []json.RawMessage
	if err := json.Unmarshal(raw, &a); err != nil {
		return true
	}
	return len(a) > 0
}

func flatLooksLikeEmail(s string) bool {
	at := strings.IndexByte(s, '@')
	return at > 0 && at < len(s)-1 && !strings.ContainsAny(s, " \t\r\n/")
}

// flatTruncate bounds an echoed file value to 64 bytes.
func flatTruncate(s string) string {
	if len(s) > 64 {
		return s[:64]
	}
	return s
}

// flatInstanceActivation runs the activation checks of a host's (or one
// registration's) instances. Prepare builds one per run: its principal set
// (P10) is filled in configuration order.
type flatInstanceActivation struct {
	host flatPrecheckHost
	// hubPrincipal returns the hub process's own ADC principal, "" when it
	// cannot be determined (P11). Nil: flatHubADCPrincipal.
	hubPrincipal func(ctx context.Context) (string, error)
	// buildCredentials builds the instance credentials from the checked
	// bytes. Nil: flatCloudRunCredentialsFromJSON.
	buildCredentials func(credType credentials.CredType, data []byte) (*auth.Credentials, error)
	// newCloudRunRuntime builds the runtime. Nil: runtime.NewFlatCloudRunRuntime.
	newCloudRunRuntime func(cfg config.CloudRunConfig, creds *auth.Credentials) (runtime.Runtime, error)
	logf               func(format string, args ...any)

	mu sync.Mutex
	// checked holds the credential bytes of instances that passed the
	// precondition, until the factory takes them.
	checked map[string]*flatCheckedCredentials
	// principals maps a claimed instance principal (lower case) to the
	// instance key that claimed it.
	principals map[string]string

	hubOnce sync.Once
	hubID   string
	hubErr  error
}

func newFlatInstanceActivation(hc flatPrecheckHost) *flatInstanceActivation {
	return &flatInstanceActivation{host: hc, checked: map[string]*flatCheckedCredentials{}, principals: map[string]string{}}
}

// precondition is the host's Precondition hook (flatInstancePrecondition).
func (a *flatInstanceActivation) precondition(ctx context.Context, inst config.V1RuntimeBrokerInstanceConfig) error {
	creds, err := flatInstancePrecheck(ctx, inst, a.host)
	if err != nil {
		return err
	}
	if creds != nil {
		a.mu.Lock()
		a.checked[inst.Key] = creds
		a.mu.Unlock()
	}
	return nil
}

// newRuntime is the host's runtime factory: cloudrun through the credential
// checks, every other type through newFlatInstanceRuntime.
func (a *flatInstanceActivation) newRuntime(ctx context.Context, inst config.V1RuntimeBrokerInstanceConfig) (runtime.Runtime, error) {
	if inst.RuntimeTarget != nil && inst.RuntimeTarget.Type == config.RuntimeTargetTypeCloudRun {
		return a.cloudRunRuntime(ctx, inst)
	}
	return newFlatInstanceRuntime(ctx, inst)
}

// cloudRunRuntime runs P7-P11 on the checked bytes, then builds the
// credentials once and the runtime with them. The credentials file is not
// opened here: without bytes from a passed precondition it refuses.
func (a *flatInstanceActivation) cloudRunRuntime(ctx context.Context, inst config.V1RuntimeBrokerInstanceConfig) (runtime.Runtime, error) {
	t := inst.RuntimeTarget
	a.mu.Lock()
	checked := a.checked[inst.Key]
	delete(a.checked, inst.Key)
	a.mu.Unlock()
	if checked == nil {
		return nil, brokerhost.WithReason(reasonCloudRunCredentialsFile,
			errors.New("the credentials file has not passed the activation file checks"))
	}
	info, err := parseFlatCloudRunCredentials(checked.data) // P7, P8
	if err != nil {
		return nil, err
	}
	if err := flatCheckPrincipalNotAgentSA(info, t.ServiceAccount); err != nil { // P9
		return nil, err
	}
	if err := a.claimPrincipal(inst.Key, info.principal); err != nil { // P10
		return nil, err
	}
	if err := a.checkNotHubPrincipal(ctx, inst.Key, info.principal); err != nil { // P11
		return nil, err
	}
	build := a.buildCredentials
	if build == nil {
		build = flatCloudRunCredentialsFromJSON
	}
	creds, err := build(info.credType, checked.data)
	if err != nil {
		return nil, brokerhost.WithReason(reasonCloudRunCredentialsType, errors.New("credentials_file could not be loaded as its credential type"))
	}
	newRT := a.newCloudRunRuntime
	if newRT == nil {
		newRT = func(cfg config.CloudRunConfig, creds *auth.Credentials) (runtime.Runtime, error) {
			return runtime.NewFlatCloudRunRuntime(cfg, creds)
		}
	}
	return newRT(config.CloudRunConfig{
		ProjectID:      t.ProjectID,
		Location:       t.Region,
		ServiceAccount: t.ServiceAccount,
		Network:        t.Network,
		Subnetwork:     t.Subnetwork,
	}, creds)
}

// claimPrincipal is P10: the first instance in configuration order to claim
// a principal keeps it; a later one is refused. The claim is recorded before
// P11 and the credential build, so an instance refused by either still holds
// its principal, and a later instance with the same principal is refused as
// shared. That is intended: two instances never share a principal.
func (a *flatInstanceActivation) claimPrincipal(key, principal string) error {
	p := strings.ToLower(principal)
	a.mu.Lock()
	defer a.mu.Unlock()
	if owner, ok := a.principals[p]; ok && owner != key {
		return brokerhost.WithReason(reasonCloudRunPrincipalShared,
			fmt.Errorf("instance %q already uses this credential principal; each cloudrun instance needs its own", owner))
	}
	a.principals[p] = key
	return nil
}

// checkNotHubPrincipal is P11: the instance principal must differ from the
// hub process's own ADC principal where that can be determined. An
// undeterminable one (including a metadata timeout) skips only this check,
// logged at info.
func (a *flatInstanceActivation) checkNotHubPrincipal(ctx context.Context, key, principal string) error {
	a.hubOnce.Do(func() {
		lookup := a.hubPrincipal
		if lookup == nil {
			lookup = flatHubADCPrincipal
		}
		a.hubID, a.hubErr = lookup(ctx)
	})
	if a.hubErr != nil || a.hubID == "" {
		logf := a.logf
		if logf == nil {
			logf = log.Printf
		}
		logf("INFO: flat Runtime Broker instance %q: the hub process's own credential principal cannot be determined; the check that the instance principal differs from it is skipped", key)
		return nil
	}
	if strings.EqualFold(a.hubID, principal) {
		return brokerhost.WithReason(reasonCloudRunPrincipalIsHub,
			errors.New("the instance credential principal is the hub process's own identity"))
	}
	return nil
}

// flatCloudRunCredentialsFromJSON builds the instance credentials of an
// accepted type from the checked bytes. The type is explicit, so the auth
// library never interprets another credential type.
func flatCloudRunCredentialsFromJSON(credType credentials.CredType, data []byte) (*auth.Credentials, error) {
	return credentials.NewCredentialsFromJSON(credType, data, &credentials.DetectOptions{Scopes: flatCloudRunScopes})
}

// flatHubADCPrincipal returns the hub process's own ADC principal where it
// can be read without using it: the email of a key-file or impersonated ADC
// file (GOOGLE_APPLICATION_CREDENTIALS, then the gcloud well-known file),
// else the hub host's own metadata server identity, bounded by a timeout.
// "" means undeterminable. It never reads an agent's metadata server.
func flatHubADCPrincipal(ctx context.Context) (string, error) {
	path := os.Getenv("GOOGLE_APPLICATION_CREDENTIALS")
	if path == "" {
		if home, err := os.UserHomeDir(); err == nil {
			wk := filepath.Join(home, ".config", "gcloud", "application_default_credentials.json")
			if _, err := os.Stat(wk); err == nil {
				path = wk
			}
		}
	}
	if path != "" {
		f, err := os.Open(path)
		if err != nil {
			return "", err
		}
		defer func() { _ = f.Close() }()
		data, err := io.ReadAll(io.LimitReader(f, flatCredentialsMaxBytes))
		if err != nil {
			return "", err
		}
		var adc struct {
			Type                           string `json:"type"`
			ClientEmail                    string `json:"client_email"`
			ServiceAccountImpersonationURL string `json:"service_account_impersonation_url"`
		}
		if err := json.Unmarshal(data, &adc); err != nil {
			return "", err
		}
		switch adc.Type {
		case string(credentials.ServiceAccount):
			return adc.ClientEmail, nil
		case string(credentials.ImpersonatedServiceAccount):
			if m := flatImpersonationURL.FindStringSubmatch(adc.ServiceAccountImpersonationURL); m != nil {
				return m[1], nil
			}
		}
		return "", nil
	}
	mctx, cancel := context.WithTimeout(ctx, flatHubMetadataTimeout)
	defer cancel()
	return metadata.NewClient(&http.Client{Timeout: flatHubMetadataTimeout}).EmailWithContext(mctx, "default")
}

// flatCloudRunScopeRuntime is what the scope probe needs from a flat
// cloudrun runtime. *runtime.CloudRunRuntime, built by
// runtime.NewFlatCloudRunRuntime (the only exported constructor of a flat
// runtime), implements it; command tests stand in for it.
type flatCloudRunScopeRuntime interface {
	ProbeProject(ctx context.Context) (*resourcemanagerpb.Project, error)
	ProbeListInstances(ctx context.Context) error
	BindProjectNumber(number string) error
	ProjectID() string
	Location() string
}

var _ flatCloudRunScopeRuntime = (*runtime.CloudRunRuntime)(nil)

// probeCloudRunExecutionScope probes a flat cloudrun instance's scope with
// its own credentials (P12): the project through Resource Manager (number
// and ACTIVE state), then a one-item list of Cloud Run instances in the
// region. Any failure is ErrExecutionScopeUnidentified with a diagnostic
// that tells denial, absence, an inactive project, an invalid region and
// connectivity apart. Then P13 refuses the probed project's default compute
// account, and the probed number is bound into the runtime. Diagnostics
// never carry credentials, the credentials-file path or the project number.
func probeCloudRunExecutionScope(ctx context.Context, inst config.V1RuntimeBrokerInstanceConfig, rt flatCloudRunScopeRuntime) (brokeridentity.ExecutionScope, error) {
	unidentified := func(format string, args ...any) (brokeridentity.ExecutionScope, error) {
		return brokeridentity.ExecutionScope{}, fmt.Errorf("%w: Cannot identify the Cloud Run execution scope: "+format+"; the instance was not activated",
			append([]any{brokeridentity.ErrExecutionScopeUnidentified}, args...)...)
	}
	pctx, cancel := context.WithTimeout(ctx, flatCloudRunProbeTimeout)
	proj, err := rt.ProbeProject(pctx)
	cancel()
	switch {
	case status.Code(err) == codes.PermissionDenied:
		return unidentified("access denied reading the configured project; the instance credential needs resourcemanager.projects.get on it")
	case status.Code(err) == codes.NotFound:
		return unidentified("the configured project was not found")
	case err != nil:
		return unidentified("reading the configured project failed (Resource Manager unreachable or the request failed): %v", status.Code(err))
	case proj == nil:
		return unidentified("reading the configured project returned nothing")
	case proj.GetState() != resourcemanagerpb.Project_ACTIVE:
		return unidentified("the configured project is not active")
	}
	number, ok := strings.CutPrefix(proj.GetName(), "projects/")
	if !ok || number == "" || strings.Trim(number, "0123456789") != "" {
		return unidentified("the configured project has no project number")
	}
	lctx, cancel := context.WithTimeout(ctx, flatCloudRunProbeTimeout)
	err = rt.ProbeListInstances(lctx)
	cancel()
	switch code := status.Code(err); {
	case err == nil:
	case code == codes.PermissionDenied:
		return unidentified("access denied listing Cloud Run instances in the configured region; the instance credential needs run.instances.list there")
	case code == codes.NotFound, code == codes.InvalidArgument:
		return unidentified("the configured region is not valid for Cloud Run instances")
	default:
		return unidentified("listing Cloud Run instances in the configured region failed (Cloud Run unreachable or the request failed): %v", code)
	}
	// P13: the static check cannot know the number.
	if sa := inst.RuntimeTarget.ServiceAccount; strings.EqualFold(sa, number+"-compute@developer.gserviceaccount.com") {
		return brokeridentity.ExecutionScope{}, brokerhost.WithReason(reasonCloudRunDefaultSA,
			errors.New("the project default service accounts are not allowed"))
	}
	if err := rt.BindProjectNumber(number); err != nil {
		return unidentified("binding the probed project failed: %v", err)
	}
	return brokeridentity.ExecutionScope{
		Type: brokeridentity.TargetTypeCloudRun,
		CloudRun: &brokeridentity.CloudRunScope{
			ProjectNumber: number,
			Region:        rt.Location(),
			ProjectID:     rt.ProjectID(),
		},
	}, nil
}

// flatCloudRunUnlabeledShown caps the instance names the OQ1 refusal lists.
const flatCloudRunUnlabeledShown = 10

// flatCloudRunUnlabeledError is the ownership preflight refusal of a flat
// cloudrun instance whose project and region hold agent instances with no
// owner label (amendment P5-CR, CR6: the drain step). It names the drain
// step and Cloud Run instance names only, at most
// flatCloudRunUnlabeledShown of them, sorted (it sorts its own copy), then
// "and M more"; the count leads. The host prefixes it with
// `flat Runtime Broker instance "<key>" not activated: `.
func flatCloudRunUnlabeledError(names []string) error {
	names = slices.Sorted(slices.Values(names))
	shown := names
	tail := ""
	if len(names) > flatCloudRunUnlabeledShown {
		shown = names[:flatCloudRunUnlabeledShown]
		tail = fmt.Sprintf(", and %d more", len(names)-flatCloudRunUnlabeledShown)
	}
	return fmt.Errorf("%d Cloud Run agent instance(s) in this project and region carry no owner label "+
		"(for example, created by a legacy Cloud Run Runtime Broker); drain them through the Runtime Broker that created them, then restart (instances: %s%s)",
		len(names), strings.Join(shown, ", "), tail)
}

// logFlatCloudRunServiceAccounts logs, to the local operator log only, the
// configured service_account of every active flat cloudrun instance, so a
// change is visible (amendment P5-CR, CR3: excluded from identity; CR8:
// never published). It is never sent to the Hub or put in the
// descriptor.
func logFlatCloudRunServiceAccounts(logf func(format string, args ...any), active []brokerhost.ActiveInstance) {
	for _, a := range active {
		t := a.Context.Instance.RuntimeTarget
		if t == nil || t.Type != config.RuntimeTargetTypeCloudRun {
			continue
		}
		logf("Flat Runtime Broker instance %q: new Cloud Run agents run as service account %s", a.Context.Instance.Key, t.ServiceAccount)
	}
}
