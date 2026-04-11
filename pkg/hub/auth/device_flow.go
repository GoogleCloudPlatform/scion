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

package auth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
)

var errDeviceFlowProviderRequired = errors.New("device flow provider is required")

// DeviceFlowAuth handles the OAuth 2.0 Device Authorization Grant flow
// for headless environments where a browser cannot be opened directly.
type DeviceFlowAuth struct {
	client           hubclient.AuthService
	output           io.Writer
	provider         string
	providerExplicit bool
}

// NewDeviceFlowAuth creates a new DeviceFlowAuth.
func NewDeviceFlowAuth(client hubclient.AuthService, provider ...string) *DeviceFlowAuth {
	selectedProvider := ""
	providerExplicit := false
	if len(provider) > 0 {
		selectedProvider = strings.TrimSpace(provider[0])
		providerExplicit = true
	}
	return &DeviceFlowAuth{
		client:           client,
		output:           os.Stdout,
		provider:         selectedProvider,
		providerExplicit: providerExplicit,
	}
}

// Authenticate runs the device authorization flow:
// 1. Requests a device code from the Hub
// 2. Displays the verification URL and user code
// 3. Polls for authorization completion
// 4. Returns the token response on success
func (d *DeviceFlowAuth) Authenticate(ctx context.Context) (*hubclient.CLITokenResponse, error) {
	if d.providerExplicit && d.provider == "" {
		return nil, errDeviceFlowProviderRequired
	}

	codeResp, provider, err := d.requestDeviceCode(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to request device code: %w", err)
	}

	fmt.Fprintf(d.output, "\nTo authenticate, visit:\n\n  %s\n\n", codeResp.VerificationURL)
	fmt.Fprintf(d.output, "And enter the code: %s\n\n", codeResp.UserCode)
	if codeResp.VerificationURLComplete != "" {
		fmt.Fprintf(d.output, "Or open this URL directly:\n  %s\n\n", codeResp.VerificationURLComplete)
	}
	fmt.Fprintf(d.output, "Waiting for authorization...\n")

	interval := time.Duration(codeResp.Interval) * time.Second
	if interval == 0 {
		interval = 5 * time.Second
	}

	deadline := time.Now().Add(time.Duration(codeResp.ExpiresIn) * time.Second)

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("device authorization expired")
		}

		pollResp, err := d.client.PollDeviceToken(ctx, codeResp.DeviceCode, provider)
		if err != nil {
			return nil, fmt.Errorf("failed to poll device token: %w", err)
		}

		switch pollResp.Status {
		case "authorization_pending":
			continue
		case "slow_down":
			interval += 5 * time.Second
			continue
		case "expired_token":
			return nil, fmt.Errorf("device authorization expired")
		case "":
			return &hubclient.CLITokenResponse{
				AccessToken:  pollResp.AccessToken,
				RefreshToken: pollResp.RefreshToken,
				ExpiresIn:    pollResp.ExpiresIn,
				User:         pollResp.User,
			}, nil
		default:
			return nil, fmt.Errorf("unexpected device token status: %s", pollResp.Status)
		}
	}
}

func (d *DeviceFlowAuth) requestDeviceCode(ctx context.Context) (*hubclient.DeviceCodeResponse, string, error) {
	providers := hubclient.OAuthProviderOrder()
	if d.providerExplicit {
		providers = []string{d.provider}
	}

	var lastErr error
	for _, provider := range providers {
		codeResp, err := d.client.RequestDeviceCode(ctx, provider)
		if err == nil {
			return codeResp, provider, nil
		}
		if !isProviderNotConfiguredError(err) {
			return nil, "", err
		}
		lastErr = err
	}

	return nil, "", lastErr
}

func isProviderNotConfiguredError(err error) bool {
	var apiErr *apiclient.APIError
	return errors.As(err, &apiErr) &&
		apiErr.Code == apiclient.ErrCodeValidationError &&
		strings.Contains(apiErr.Message, "OAuth provider not configured for device flow")
}
