package imagecheck

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const defaultHTTPTimeout = 10 * time.Second

type HTTPClient interface {
	Do(req *http.Request) (*http.Response, error)
}

type imageRef struct {
	Registry   string
	Repository string
	Tag        string
}

func parseImageRef(image string) (imageRef, error) {
	if image == "" {
		return imageRef{}, fmt.Errorf("empty image reference")
	}

	ref := image
	tag := "latest"
	if i := strings.LastIndex(ref, ":"); i > 0 {
		possibleTag := ref[i+1:]
		if !strings.Contains(possibleTag, "/") {
			tag = possibleTag
			ref = ref[:i]
		}
	}

	registry := "docker.io"
	repo := ref

	parts := strings.SplitN(ref, "/", 2)
	if len(parts) == 2 && (strings.Contains(parts[0], ".") || strings.Contains(parts[0], ":")) {
		registry = parts[0]
		repo = parts[1]
	} else if len(parts) == 1 {
		repo = "library/" + parts[0]
	}

	return imageRef{
		Registry:   registry,
		Repository: repo,
		Tag:        tag,
	}, nil
}

func registryHost(registry string) string {
	if registry == "docker.io" {
		return "registry-1.docker.io"
	}
	return registry
}

func checkRemoteImage(ctx context.Context, client HTTPClient, ref imageRef, now time.Time) CheckResult {
	host := registryHost(ref.Registry)
	manifestURL := fmt.Sprintf("https://%s/v2/%s/manifests/%s", host, ref.Repository, ref.Tag)

	slog.Debug("image check: probing remote registry", "url", manifestURL, "registry", ref.Registry, "repo", ref.Repository, "tag", ref.Tag)

	result := doRegistryHead(ctx, client, manifestURL, "", now)
	if result.Status != "" {
		return result
	}

	return CheckResult{
		Status:    "error",
		Error:     "unexpected empty result",
		CheckedAt: now,
	}
}

func doRegistryHead(ctx context.Context, client HTTPClient, url, token string, now time.Time) CheckResult {
	req, err := http.NewRequestWithContext(ctx, "HEAD", url, nil)
	if err != nil {
		return CheckResult{
			Status:    "error",
			Error:     err.Error(),
			CheckedAt: now,
		}
	}
	req.Header.Set("Accept", strings.Join([]string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
	}, ", "))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := client.Do(req)
	if err != nil {
		return CheckResult{
			Status:    "error",
			Error:     err.Error(),
			CheckedAt: now,
		}
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		return CheckResult{
			Status:    "valid",
			Source:    "registry",
			Hash:      resp.Header.Get("Docker-Content-Digest"),
			CheckedAt: now,
		}
	case http.StatusNotFound:
		return CheckResult{
			Status:    "invalid",
			CheckedAt: now,
		}
	case http.StatusUnauthorized:
		if token != "" {
			slog.Warn("image check: registry auth failed after token exchange", "url", url)
			return CheckResult{
				Status:    "error",
				Error:     "registry requires authentication (token rejected)",
				CheckedAt: now,
			}
		}
		anonToken, err := fetchAnonymousToken(ctx, client, resp.Header.Get("Www-Authenticate"))
		if err != nil {
			slog.Warn("image check: anonymous token fetch failed", "url", url, "error", err)
			return CheckResult{
				Status:    "error",
				Error:     fmt.Sprintf("registry auth: %v", err),
				CheckedAt: now,
			}
		}
		return doRegistryHead(ctx, client, url, anonToken, now)
	default:
		return CheckResult{
			Status:    "error",
			Error:     fmt.Sprintf("registry returned %d", resp.StatusCode),
			CheckedAt: now,
		}
	}
}

func fetchAnonymousToken(ctx context.Context, client HTTPClient, wwwAuth string) (string, error) {
	if wwwAuth == "" {
		return "", fmt.Errorf("no Www-Authenticate header")
	}

	realm, params := parseWWWAuthenticate(wwwAuth)
	if realm == "" {
		return "", fmt.Errorf("no realm in Www-Authenticate header")
	}

	tokenURL := realm
	sep := "?"
	for k, v := range params {
		tokenURL += sep + url.QueryEscape(k) + "=" + url.QueryEscape(v)
		sep = "&"
	}

	req, err := http.NewRequestWithContext(ctx, "GET", tokenURL, nil)
	if err != nil {
		return "", err
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}

	var tokenResp struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", err
	}

	if tokenResp.Token != "" {
		return tokenResp.Token, nil
	}
	if tokenResp.AccessToken != "" {
		return tokenResp.AccessToken, nil
	}
	return "", fmt.Errorf("no token in response")
}

func parseWWWAuthenticate(header string) (string, map[string]string) {
	params := make(map[string]string)
	header = strings.TrimPrefix(header, "Bearer ")
	header = strings.TrimPrefix(header, "bearer ")

	var realm string
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		v = strings.Trim(v, "\"")
		if k == "realm" {
			realm = v
		} else {
			params[k] = v
		}
	}
	return realm, params
}
