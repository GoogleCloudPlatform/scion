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
)

var (
	SandboxGVR      = schema.GroupVersionResource{Group: "agents.x-k8s.io", Version: "v1alpha1", Resource: "sandboxes"}
	SandboxClaimGVR = schema.GroupVersionResource{Group: "extensions.agents.x-k8s.io", Version: "v1alpha1", Resource: "sandboxclaims"}

	// SecretProviderClassGVR is the GVR for the Secrets Store CSI Driver SecretProviderClass CRD.
	SecretProviderClassGVR = schema.GroupVersionResource{
		Group: "secrets-store.csi.x-k8s.io", Version: "v1", Resource: "secretproviderclasses",
	}
)

type Client struct {
	dynamic        dynamic.Interface
	Clientset      kubernetes.Interface
	Config         *rest.Config
	CurrentContext string
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
	loadConfig func(string, string) (*rest.Config, string, error),
	inClusterConfig func() (*rest.Config, error),
	newDynamicClient func(*rest.Config) (dynamic.Interface, error),
	newClientset func(*rest.Config) (kubernetes.Interface, error),
) (*Client, error) {
	config, currentContext, err := loadConfig(kubeconfigPath, contextName)
	if err != nil {
		kubeconfigErr := err
		if kubeconfigPath == "" && contextName == "" {
			config, err = inClusterConfig()
			if err == nil {
				return newClientFromConfig(config, "in-cluster", newDynamicClient, newClientset)
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

	return newClientFromConfig(config, currentContext, newDynamicClient, newClientset)
}

func loadClientConfig(kubeconfigPath, contextName string) (*rest.Config, string, error) {
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
		return nil, "", err
	}

	currentContext := ""
	rawConfig, err := configLoader.RawConfig()
	if err == nil {
		if contextName != "" {
			currentContext = contextName
		} else {
			currentContext = rawConfig.CurrentContext
		}
	}

	return config, currentContext, nil
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
// Verify attempts to fall back to GCE metadata-based auth when running on
// a GCE instance.
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

	// On GCE, transparently fall back to metadata-based auth instead of
	// requiring gcloud/exec plugins to be configured in the process env.
	if metadata.OnGCE() {
		slog.Info("Exec credential plugin failed, falling back to GCE metadata auth",
			"original_error", errMsg)
		if fallbackErr := c.fallbackToGCEAuth(); fallbackErr != nil {
			return fmt.Errorf("exec credential plugin failed and GCE metadata auth fallback also failed: %v — original error: %w", fallbackErr, err)
		}
		return nil
	}

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
	return fmt.Errorf("%s — underlying error: %w", hint, err)
}

// fallbackToGCEAuth reconfigures the client to use GCE metadata-based
// OAuth2 tokens instead of the exec-based credential plugin. This is the
// standard auth method for services running on GCE/GKE infrastructure.
func (c *Client) fallbackToGCEAuth() error {
	ctx := context.Background()
	ts, err := google.DefaultTokenSource(ctx, "https://www.googleapis.com/auth/cloud-platform")
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
		return fmt.Errorf("GCE metadata auth connected but cluster rejected credentials: %w", err)
	}

	slog.Info("Successfully authenticated to Kubernetes via GCE metadata")
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
