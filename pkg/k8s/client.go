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

package k8s

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"cloud.google.com/go/compute/metadata"
	"github.com/GoogleCloudPlatform/scion/pkg/k8s/api/v1alpha1"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

var (
	SandboxGVR      = schema.GroupVersionResource{Group: "agents.x-k8s.io", Version: "v1alpha1", Resource: "sandboxes"}
	SandboxClaimGVR = schema.GroupVersionResource{Group: "extensions.agents.x-k8s.io", Version: "v1alpha1", Resource: "sandboxclaims"}

	// SecretProviderClassGVR is the GVR for the Secrets Store CSI Driver SecretProviderClass CRD.
	SecretProviderClassGVR = schema.GroupVersionResource{
		Group: "secrets-store.csi.x-k8s.io", Version: "v1", Resource: "secretproviderclasses",
	}
)

// ConnectionMode is how a Client connected when it was built: through a
// kubeconfig, or with in-cluster credentials. It is set at construction
// from what the client actually used and never changes afterwards.
type ConnectionMode int

const (
	// ConnUnknown means the source cannot be told from what was loaded
	// (zero value).
	ConnUnknown ConnectionMode = iota
	// ConnKubeconfig means the client was built from a kubeconfig context.
	ConnKubeconfig
	// ConnInCluster means the client uses the in-cluster service account
	// credentials of the pod it runs in.
	ConnInCluster
)

// String returns a short name for the mode.
func (m ConnectionMode) String() string {
	switch m {
	case ConnKubeconfig:
		return "kubeconfig"
	case ConnInCluster:
		return "in-cluster"
	default:
		return "unknown"
	}
}

type Client struct {
	dynamic        dynamic.Interface
	Clientset      kubernetes.Interface
	Config         *rest.Config
	CurrentContext string

	// Connection is how the client connected, set at construction.
	Connection ConnectionMode

	// RawCurrentContext is the kubeconfig's own current-context as loaded
	// when the client was built. It differs from CurrentContext for a
	// client built with an explicit context (CurrentContext is then the
	// requested name), and is "" for an in-cluster client.
	RawCurrentContext string

	// explicitFile is set for a client built from exactly one kubeconfig
	// file (NewClientFromKubeconfigFile): the file selects the credential
	// source too, so Verify never substitutes Application Default
	// Credentials when the file's exec credential plugin fails.
	explicitFile bool
}

// NewClient creates a Kubernetes client using the default or specified kubeconfig.
// It uses the current context from the kubeconfig unless overridden via NewClientWithContext.
func NewClient(kubeconfigPath string) (*Client, error) {
	return NewClientWithContext(kubeconfigPath, "")
}

// NewClientWithContext creates a Kubernetes client targeting a specific context.
// If contextName is empty, the current context from the kubeconfig is used.
func NewClientWithContext(kubeconfigPath, contextName string) (*Client, error) {
	return newClientWithContext(
		kubeconfigPath,
		contextName,
		loadClientConfig,
		rest.InClusterConfig,
		newDynamicForConfig,
		newClientsetForConfig,
	)
}

// errInClusterDisabled is the in-cluster leg's error for
// NewClientWithContextNoInCluster.
var errInClusterDisabled = errors.New("in-cluster config disabled for this client")

// NewClientWithContextNoInCluster is NewClientWithContext without any
// in-cluster fallback: only the default loading rules (KUBECONFIG, including
// a path list, and ~/.kube/config) or kubeconfigPath. It is for callers, such
// as tests, that must never use the service account of the pod they run in.
// When no kubeconfig yields a configuration, the error wraps
// errInClusterDisabled.
func NewClientWithContextNoInCluster(kubeconfigPath, contextName string) (*Client, error) {
	return newClientWithContext(
		kubeconfigPath,
		contextName,
		loadClientConfigNoInCluster,
		func() (*rest.Config, error) { return nil, errInClusterDisabled },
		newDynamicForConfig,
		newClientsetForConfig,
	)
}

// newDynamicForConfig and newClientsetForConfig are the API client
// constructors NewClientWithContext and NewClientWithContextNoInCluster
// share.
func newDynamicForConfig(config *rest.Config) (dynamic.Interface, error) {
	return dynamic.NewForConfig(config)
}

func newClientsetForConfig(config *rest.Config) (kubernetes.Interface, error) {
	return kubernetes.NewForConfig(config)
}

// NewClientFromKubeconfigFile creates a client from exactly one kubeconfig
// file, for a caller that must never use another source: no default loading
// rules, no KUBECONFIG, and no in-cluster fallback (the deferred loader can
// fall back to in-cluster credentials when the explicit file yields an
// empty configuration). An empty file, a file with no usable context, or a
// context it does not define is an error. If contextName is empty, the
// file's current context is used.
func NewClientFromKubeconfigFile(path, contextName string) (*Client, error) {
	cfg, err := clientcmd.LoadFromFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to load kubeconfig: %w", err)
	}
	// Relative paths in the file (certificates, keys, token files, exec
	// commands) stay relative to the file, as the loading rules resolve them.
	if err := clientcmd.ResolveLocalPaths(cfg); err != nil {
		return nil, fmt.Errorf("failed to load kubeconfig: %w", err)
	}
	restCfg, err := newExplicitClientConfig(cfg, contextName).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to load kubeconfig: %w", err)
	}
	current := contextName
	if current == "" {
		current = cfg.CurrentContext
	}
	c, err := newClientFromConfig(restCfg, current,
		func(config *rest.Config) (dynamic.Interface, error) { return dynamic.NewForConfig(config) },
		func(config *rest.Config) (kubernetes.Interface, error) { return kubernetes.NewForConfig(config) })
	if err != nil {
		return nil, err
	}
	c.explicitFile = true
	// The file is the only source, so this is a kubeconfig connection.
	c.Connection = ConnKubeconfig
	c.RawCurrentContext = cfg.CurrentContext
	return c, nil
}

// newExplicitClientConfig is the configuration constructor
// NewClientFromKubeconfigFile uses (explicitFileClientConfig); a variable
// so a test can observe that the explicit path goes through it.
var newExplicitClientConfig = explicitFileClientConfig

// explicitFileClientConfig is the client configuration of one loaded
// kubeconfig file: a direct configuration with no loading rules and no
// in-cluster leg (unlike the deferred loader, which falls back to
// in-cluster credentials when its sources yield an empty configuration).
func explicitFileClientConfig(cfg *clientcmdapi.Config, contextName string) clientcmd.ClientConfig {
	return clientcmd.NewNonInteractiveClientConfig(*cfg, contextName,
		&clientcmd.ConfigOverrides{CurrentContext: contextName}, nil)
}

// NewClientWithContextTimeout is NewClientWithContext with every request
// of the client bounded by timeout (rest.Config.Timeout). That includes
// Verify, and the Application Default Credentials fallback Verify may
// switch to, since the fallback copies the client's config.
func NewClientWithContextTimeout(kubeconfigPath, contextName string, timeout time.Duration) (*Client, error) {
	withTimeout := func(config *rest.Config) *rest.Config {
		config.Timeout = timeout
		return config
	}
	return newClientWithContext(
		kubeconfigPath,
		contextName,
		func(path, name string) (*rest.Config, loadedSource, error) {
			config, src, err := loadClientConfig(path, name)
			if err != nil {
				return nil, loadedSource{}, err
			}
			return withTimeout(config), src, nil
		},
		func() (*rest.Config, error) {
			config, err := rest.InClusterConfig()
			if err != nil {
				return nil, err
			}
			return withTimeout(config), nil
		},
		func(config *rest.Config) (dynamic.Interface, error) {
			return dynamic.NewForConfig(config)
		},
		func(config *rest.Config) (kubernetes.Interface, error) {
			return kubernetes.NewForConfig(config)
		},
	)
}

func newClientWithContext(
	kubeconfigPath,
	contextName string,
	loadConfig func(string, string) (*rest.Config, loadedSource, error),
	inClusterConfig func() (*rest.Config, error),
	newDynamicClient func(*rest.Config) (dynamic.Interface, error),
	newClientset func(*rest.Config) (kubernetes.Interface, error),
) (*Client, error) {
	config, src, err := loadConfig(kubeconfigPath, contextName)
	if err != nil {
		kubeconfigErr := err
		if kubeconfigPath == "" && contextName == "" {
			config, err = inClusterConfig()
			if err == nil {
				c, err := newClientFromConfig(config, "in-cluster", newDynamicClient, newClientset)
				if err != nil {
					return nil, err
				}
				c.Connection = ConnInCluster
				return c, nil
			}
			return nil, fmt.Errorf("failed to load kubeconfig and in-cluster config: %w",
				errors.Join(
					fmt.Errorf("kubeconfig: %w", kubeconfigErr),
					fmt.Errorf("in-cluster: %w", err),
				),
			)
		}
		return nil, fmt.Errorf("failed to load kubeconfig: %w", kubeconfigErr)
	}

	c, err := newClientFromConfig(config, src.currentContext, newDynamicClient, newClientset)
	if err != nil {
		return nil, err
	}
	c.Connection = src.connection
	c.RawCurrentContext = src.rawCurrentContext
	return c, nil
}

// loadedSource describes what a kubeconfig loader used: the client's
// current context (the requested name when one was given), the raw
// kubeconfig current-context, and the connection mode.
type loadedSource struct {
	currentContext    string
	rawCurrentContext string
	connection        ConnectionMode
}

// connectionModeFor classifies a deferred-loader client that loaded
// successfully. It only reports Kubeconfig when the raw config fully
// describes the selected cluster: the selected context (the requested one,
// else the raw current-context) exists, its cluster exists, and that
// cluster has a server. A raw configuration with no current context, no
// contexts and no clusters means the deferred loader's sources were empty,
// so its ClientConfig fell back to in-cluster credentials. Anything else
// (the raw configuration could not be read, or it names a context or
// cluster it does not fully describe, in which case the loader may also
// have fallen back to in-cluster credentials) is unknown.
func connectionModeFor(contextName string, raw clientcmdapi.Config, rawErr error) ConnectionMode {
	if rawErr != nil {
		return ConnUnknown
	}
	selected := contextName
	if selected == "" {
		selected = raw.CurrentContext
	}
	if selected == "" {
		if len(raw.Contexts) == 0 && len(raw.Clusters) == 0 {
			return ConnInCluster
		}
		return ConnUnknown
	}
	ctx, ok := raw.Contexts[selected]
	if !ok || ctx == nil {
		return ConnUnknown
	}
	cluster, ok := raw.Clusters[ctx.Cluster]
	if !ok || cluster == nil || cluster.Server == "" {
		return ConnUnknown
	}
	return ConnKubeconfig
}

func loadClientConfig(kubeconfigPath, contextName string) (*rest.Config, loadedSource, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfigPath != "" {
		loadingRules.ExplicitPath = kubeconfigPath
	}

	overrides := &clientcmd.ConfigOverrides{}
	if contextName != "" {
		overrides.CurrentContext = contextName
	}

	configLoader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loadingRules,
		overrides,
	)

	config, err := configLoader.ClientConfig()
	if err != nil {
		return nil, loadedSource{}, err
	}

	src := loadedSource{}
	rawConfig, rawErr := configLoader.RawConfig()
	if rawErr == nil {
		src.rawCurrentContext = rawConfig.CurrentContext
		if contextName != "" {
			src.currentContext = contextName
		} else {
			src.currentContext = rawConfig.CurrentContext
		}
	}
	src.connection = connectionModeFor(contextName, rawConfig, rawErr)

	return config, src, nil
}

// loadClientConfigNoInCluster is loadClientConfig with no in-cluster leg;
// the two differ only there. loadClientConfig uses client-go's deferred
// loader, whose ClientConfig (clientcmd.DeferredLoadingClientConfig) returns
// the in-cluster configuration itself when the kubeconfig sources yield an
// empty or default configuration and an in-cluster config is possible (the
// service variables and token file of a pod). This loads the same sources,
// with the same loading rules and overrides, and builds a direct client
// configuration from them, which has no in-cluster leg by construction. An
// empty configuration, or a context the merged configuration does not
// define, is an error.
func loadClientConfigNoInCluster(kubeconfigPath, contextName string) (*rest.Config, loadedSource, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfigPath != "" {
		loadingRules.ExplicitPath = kubeconfigPath
	}
	raw, err := loadingRules.Load()
	if err != nil {
		return nil, loadedSource{}, err
	}
	overrides := &clientcmd.ConfigOverrides{}
	if contextName != "" {
		overrides.CurrentContext = contextName
	}
	config, err := clientcmd.NewNonInteractiveClientConfig(*raw, contextName, overrides, loadingRules).ClientConfig()
	if err != nil {
		return nil, loadedSource{}, err
	}
	currentContext := contextName
	if currentContext == "" {
		currentContext = raw.CurrentContext
	}
	// No in-cluster leg here, so a load that succeeds is a kubeconfig
	// connection when a context was requested or the file sets one.
	conn := ConnUnknown
	if currentContext != "" {
		conn = ConnKubeconfig
	}
	return config, loadedSource{currentContext: currentContext, rawCurrentContext: raw.CurrentContext, connection: conn}, nil
}

func newClientFromConfig(
	config *rest.Config,
	currentContext string,
	newDynamicClient func(*rest.Config) (dynamic.Interface, error),
	newClientset func(*rest.Config) (kubernetes.Interface, error),
) (*Client, error) {
	dynClient, err := newDynamicClient(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create dynamic client: %w", err)
	}

	clientset, err := newClientset(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create kubernetes clientset: %w", err)
	}

	return &Client{
		dynamic:        dynClient,
		Clientset:      clientset,
		Config:         config,
		CurrentContext: currentContext,
	}, nil
}

// Verify performs a lightweight API call (ServerVersion) to validate that
// cluster connectivity and credentials work. If the kubeconfig uses an
// exec-based credential plugin (e.g. gke-gcloud-auth-plugin) and it fails,
// Verify attempts to fall back to Application Default Credentials when
// running on a GCE instance.
func (c *Client) Verify() error {
	_, err := c.Clientset.Discovery().ServerVersion()
	if err == nil {
		return nil
	}

	errMsg := err.Error()

	// Detect exec-based credential plugin failures.
	if !strings.Contains(errMsg, "getting credentials: exec:") {
		return fmt.Errorf("failed to connect to Kubernetes cluster: %w", err)
	}

	// A client built from one explicit kubeconfig file uses only the
	// credential source that file selects: no Application Default
	// Credentials substitution.
	if c.explicitFile {
		return fmt.Errorf("the exec credential plugin of the explicit kubeconfig failed (no other credential source is used for this kubeconfig). %s — underlying error: %w",
			credentialPluginHint(errMsg), err)
	}

	// On GCE, transparently fall back to Application Default Credentials
	// instead of requiring gcloud/exec plugins to be configured in the
	// process env.
	if metadata.OnGCE() {
		slog.Info("Exec credential plugin failed, falling back to Application Default Credentials (ADC) auth",
			"original_error", errMsg)
		if fallbackErr := c.fallbackToGCEAuth(); fallbackErr != nil {
			return fmt.Errorf("exec credential plugin failed and Application Default Credentials (ADC) auth fallback also failed: %v — original error: %w", fallbackErr, err)
		}
		return nil
	}

	return fmt.Errorf("%s — underlying error: %w", credentialPluginHint(errMsg), err)
}

// credentialPluginHint is the operator hint for a failed exec credential
// plugin.
func credentialPluginHint(errMsg string) string {
	hint := "Kubernetes credential plugin failed. "
	if strings.Contains(errMsg, "gke-gcloud-auth-plugin") {
		hint += "The gke-gcloud-auth-plugin could not obtain credentials. " +
			"Ensure the hub/broker process inherits the same environment as your shell " +
			"(HOME, PATH, CLOUDSDK_CONFIG, GOOGLE_APPLICATION_CREDENTIALS). " +
			"If running as a systemd service, verify these are set in the unit file. " +
			"You can test with: gke-gcloud-auth-plugin --version"
	} else {
		hint += "Ensure the credential plugin is installed and the process environment " +
			"includes the necessary variables (HOME, PATH, cloud SDK config)."
	}
	return hint
}

// gceFallbackAuthScopes are the OAuth2 scopes requested from Application
// Default Credentials (a service account key file, the gcloud ADC file, or
// the GCE/GKE metadata server — whichever DefaultTokenSource resolves first)
// when falling back from the exec credential plugin. cloud-platform alone
// authenticates the caller to GCP, but GKE authorizes RBAC subjects by
// identity: without userinfo.email, tokens are presented to the cluster
// under the service account's numeric unique ID rather than its email
// address, so email-subject RoleBindings never match.
//
// On a plain GCE VM (not GKE Workload Identity), the metadata server can
// only mint scopes within the instance's (or node pool's) configured access
// scopes; the VM must already include userinfo.email, or be granted it, for
// this to take effect there. The GKE metadata server backing Workload
// Identity honors the requested scopes directly. Requesting the extra scope
// never narrows what cloud-platform alone would grant.
var gceFallbackAuthScopes = []string{
	"https://www.googleapis.com/auth/cloud-platform",
	"https://www.googleapis.com/auth/userinfo.email",
}

// defaultTokenSource resolves Application Default Credentials for the
// requested scopes. A package var so tests can stub it and assert on the
// scopes fallbackToGCEAuth actually passes, rather than only on the
// gceFallbackAuthScopes literal.
var defaultTokenSource = google.DefaultTokenSource

// fallbackToGCEAuth reconfigures the client to use Application Default
// Credentials instead of the exec-based credential plugin. This is the
// standard auth method for services running on GCE/GKE infrastructure.
func (c *Client) fallbackToGCEAuth() error {
	ctx := context.Background()
	ts, err := defaultTokenSource(ctx, gceFallbackAuthScopes...)
	if err != nil {
		return fmt.Errorf("failed to get default token source: %w", err)
	}

	newConfig := rest.CopyConfig(c.Config)
	newConfig.ExecProvider = nil
	newConfig.AuthProvider = nil
	newConfig.BearerToken = ""
	newConfig.BearerTokenFile = ""
	newConfig.WrapTransport = func(rt http.RoundTripper) http.RoundTripper {
		return &oauth2.Transport{Source: ts, Base: rt}
	}

	newClientset, err := kubernetes.NewForConfig(newConfig)
	if err != nil {
		return fmt.Errorf("failed to create clientset with GCE auth: %w", err)
	}

	newDynamic, err := dynamic.NewForConfig(newConfig)
	if err != nil {
		return fmt.Errorf("failed to create dynamic client with GCE auth: %w", err)
	}

	// Verify the fallback actually works
	if _, err := newClientset.Discovery().ServerVersion(); err != nil {
		return fmt.Errorf("ADC auth connected but cluster rejected credentials: %w", err)
	}

	slog.Info("Successfully authenticated to Kubernetes via Application Default Credentials")
	c.Clientset = newClientset
	c.dynamic = newDynamic
	c.Config = newConfig
	return nil
}

// IsGKE returns true if the connected cluster is a GKE cluster, detected by
// the presence of "-gke." in the server version string (e.g. "v1.28.3-gke.1286000").
func (c *Client) IsGKE() bool {
	ver, err := c.Clientset.Discovery().ServerVersion()
	if err != nil {
		return false
	}
	return strings.Contains(ver.GitVersion, "-gke.")
}

func NewTestClient(dyn dynamic.Interface, cs kubernetes.Interface) *Client {
	return &Client{
		dynamic:   dyn,
		Clientset: cs,
	}
}

// Dynamic returns the dynamic Kubernetes client for CRD operations.
func (c *Client) Dynamic() dynamic.Interface { return c.dynamic }

func (c *Client) CreateSandboxClaim(ctx context.Context, namespace string, claim *v1alpha1.SandboxClaim) (*v1alpha1.SandboxClaim, error) {
	unstructuredMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(claim)
	if err != nil {
		return nil, fmt.Errorf("failed to convert claim to unstructured: %w", err)
	}

	u := &unstructured.Unstructured{Object: unstructuredMap}
	u.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   v1alpha1.ExtensionsGroupVersion.Group,
		Version: v1alpha1.ExtensionsGroupVersion.Version,
		Kind:    "SandboxClaim",
	})

	result, err := c.dynamic.Resource(SandboxClaimGVR).Namespace(namespace).Create(ctx, u, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}

	var createdClaim v1alpha1.SandboxClaim
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(result.Object, &createdClaim); err != nil {
		return nil, fmt.Errorf("failed to convert result to claim: %w", err)
	}

	return &createdClaim, nil
}

func (c *Client) GetSandboxClaim(ctx context.Context, namespace, name string) (*v1alpha1.SandboxClaim, error) {
	result, err := c.dynamic.Resource(SandboxClaimGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	var claim v1alpha1.SandboxClaim
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(result.Object, &claim); err != nil {
		return nil, fmt.Errorf("failed to convert result to claim: %w", err)
	}

	return &claim, nil
}

func (c *Client) ListSandboxClaims(ctx context.Context, namespace string, labelSelector string) (*v1alpha1.SandboxClaimList, error) {
	result, err := c.dynamic.Resource(SandboxClaimGVR).Namespace(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		return nil, err
	}

	var claimList v1alpha1.SandboxClaimList
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(result.Object, &claimList); err != nil {
		return nil, fmt.Errorf("failed to convert result to claim list: %w", err)
	}

	// Workaround for FromUnstructured not populating Items from dynamic client list
	if len(claimList.Items) == 0 && len(result.Items) > 0 {
		claimList.Items = make([]v1alpha1.SandboxClaim, len(result.Items))
		for i, item := range result.Items {
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(item.Object, &claimList.Items[i]); err != nil {
				return nil, fmt.Errorf("failed to convert item %d: %w", i, err)
			}
		}
	}

	return &claimList, nil
}

func (c *Client) DeleteSandboxClaim(ctx context.Context, namespace, name string) error {
	return c.dynamic.Resource(SandboxClaimGVR).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{})
}

func (c *Client) GetSandbox(ctx context.Context, namespace, name string) (*v1alpha1.Sandbox, error) {
	result, err := c.dynamic.Resource(SandboxGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	var sandbox v1alpha1.Sandbox
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(result.Object, &sandbox); err != nil {
		return nil, fmt.Errorf("failed to convert result to sandbox: %w", err)
	}

	return &sandbox, nil
}

// CreateSecretProviderClass creates a SecretProviderClass CRD resource in the given namespace.
func (c *Client) CreateSecretProviderClass(ctx context.Context, namespace string, spc *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	return c.dynamic.Resource(SecretProviderClassGVR).Namespace(namespace).Create(ctx, spc, metav1.CreateOptions{})
}

// DeleteSecretProviderClass deletes a SecretProviderClass CRD resource by name.
func (c *Client) DeleteSecretProviderClass(ctx context.Context, namespace, name string) error {
	return c.dynamic.Resource(SecretProviderClassGVR).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{})
}

// ListSecretProviderClasses lists SecretProviderClass CRD resources matching a label selector.
func (c *Client) ListSecretProviderClasses(ctx context.Context, namespace, labelSelector string) (*unstructured.UnstructuredList, error) {
	return c.dynamic.Resource(SecretProviderClassGVR).Namespace(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labelSelector,
	})
}
