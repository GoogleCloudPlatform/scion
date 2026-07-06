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

package secret

import (
	"context"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	smpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
	"google.golang.org/api/option"
)

// SMClient is the interface for interacting with GCP Secret Manager.
// It is defined as an interface to allow mocking in tests.
type SMClient interface {
	CreateSecret(ctx context.Context, req *smpb.CreateSecretRequest) (*smpb.Secret, error)
	AddSecretVersion(ctx context.Context, req *smpb.AddSecretVersionRequest) (*smpb.SecretVersion, error)
	AccessSecretVersion(ctx context.Context, req *smpb.AccessSecretVersionRequest) (*smpb.AccessSecretVersionResponse, error)
	DeleteSecret(ctx context.Context, req *smpb.DeleteSecretRequest) error
	GetSecret(ctx context.Context, req *smpb.GetSecretRequest) (*smpb.Secret, error)
	Close() error
}

// gcpSMClient wraps the real GCP Secret Manager client to implement SMClient.
type gcpSMClient struct {
	client *secretmanager.Client
}

func newGCPSMClient(ctx context.Context, credentialsJSON string) (SMClient, error) {
	var opts []option.ClientOption
	if credentialsJSON != "" {
		opts = append(opts, option.WithAuthCredentialsJSON(option.ServiceAccount, []byte(credentialsJSON)))
	}
	client, err := secretmanager.NewClient(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return &gcpSMClient{client: client}, nil
}

func (c *gcpSMClient) CreateSecret(ctx context.Context, req *smpb.CreateSecretRequest) (*smpb.Secret, error) {
	return c.client.CreateSecret(ctx, req)
}

func (c *gcpSMClient) AddSecretVersion(ctx context.Context, req *smpb.AddSecretVersionRequest) (*smpb.SecretVersion, error) {
	return c.client.AddSecretVersion(ctx, req)
}

func (c *gcpSMClient) AccessSecretVersion(ctx context.Context, req *smpb.AccessSecretVersionRequest) (*smpb.AccessSecretVersionResponse, error) {
	return c.client.AccessSecretVersion(ctx, req)
}

func (c *gcpSMClient) DeleteSecret(ctx context.Context, req *smpb.DeleteSecretRequest) error {
	return c.client.DeleteSecret(ctx, req)
}

func (c *gcpSMClient) GetSecret(ctx context.Context, req *smpb.GetSecretRequest) (*smpb.Secret, error) {
	return c.client.GetSecret(ctx, req)
}

func (c *gcpSMClient) Close() error {
	return c.client.Close()
}
