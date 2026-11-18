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

package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	smpb "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
)

func TestSecretPayloadData(t *testing.T) {
	tests := []struct {
		name    string
		resp    *smpb.AccessSecretVersionResponse
		want    string
		wantErr bool
	}{
		{name: "nil response", resp: nil, wantErr: true},
		{name: "nil payload", resp: &smpb.AccessSecretVersionResponse{Name: "projects/p/secrets/s/versions/1"}, wantErr: true},
		{name: "payload with data", resp: &smpb.AccessSecretVersionResponse{Payload: &smpb.SecretPayload{Data: []byte("FAKE-value")}}, want: "FAKE-value"},
		{name: "payload with empty data", resp: &smpb.AccessSecretVersionResponse{Payload: &smpb.SecretPayload{}}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := secretPayloadData(tt.resp)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("secretPayloadData() = %q, nil; want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("secretPayloadData() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("secretPayloadData() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAccessSecretVersion(t *testing.T) {
	tests := []struct {
		name         string
		resourceName string
		resp         *smpb.AccessSecretVersionResponse
		accessErr    error
		wantReqName  string
		want         string
		wantErr      string
	}{
		{
			name:         "nil response",
			resourceName: "projects/p/secrets/s",
			resp:         nil,
			wantReqName:  "projects/p/secrets/s/versions/latest",
			wantErr:      "secret manager returned no payload",
		},
		{
			name:         "nil payload",
			resourceName: "projects/p/secrets/s",
			resp:         &smpb.AccessSecretVersionResponse{Name: "projects/p/secrets/s/versions/1"},
			wantReqName:  "projects/p/secrets/s/versions/latest",
			wantErr:      "secret manager returned no payload",
		},
		{
			name:         "payload with data and whitespace",
			resourceName: "projects/p/secrets/s",
			resp: &smpb.AccessSecretVersionResponse{
				Payload: &smpb.SecretPayload{Data: []byte("  RkFLRS12YWx1ZQ==\n")},
			},
			wantReqName: "projects/p/secrets/s/versions/latest",
			want:        "RkFLRS12YWx1ZQ==",
		},
		{
			name:         "explicit version preserved",
			resourceName: "projects/p/secrets/s/versions/3",
			resp: &smpb.AccessSecretVersionResponse{
				Payload: &smpb.SecretPayload{Data: []byte("RkFLRS12YWx1ZQ==")},
			},
			wantReqName: "projects/p/secrets/s/versions/3",
			want:        "RkFLRS12YWx1ZQ==",
		},
		{
			name:         "payload with empty data",
			resourceName: "projects/p/secrets/s",
			resp: &smpb.AccessSecretVersionResponse{
				Payload: &smpb.SecretPayload{},
			},
			wantReqName: "projects/p/secrets/s/versions/latest",
			want:        "",
		},
		{
			name:         "non-ascii payload rejected by cleanBase64",
			resourceName: "projects/p/secrets/s",
			resp: &smpb.AccessSecretVersionResponse{
				Payload: &smpb.SecretPayload{Data: []byte{0xff, 'A'}},
			},
			wantReqName: "projects/p/secrets/s/versions/latest",
			wantErr:     "non-ASCII byte",
		},
		{
			name:         "access rpc error",
			resourceName: "projects/p/secrets/s",
			accessErr:    errors.New("permission denied"),
			wantReqName:  "projects/p/secrets/s/versions/latest",
			wantErr:      "permission denied",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotReqName string
			got, err := accessSecretVersion(context.Background(), tt.resourceName, func(_ context.Context, req *smpb.AccessSecretVersionRequest) (*smpb.AccessSecretVersionResponse, error) {
				gotReqName = req.GetName()
				return tt.resp, tt.accessErr
			})
			if gotReqName != tt.wantReqName {
				t.Fatalf("request Name = %q, want %q", gotReqName, tt.wantReqName)
			}
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("accessSecretVersion() = %q, nil; want error containing %q", got, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("accessSecretVersion() error = %q, want substring %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("accessSecretVersion() unexpected error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("accessSecretVersion() = %q, want %q", got, tt.want)
			}
		})
	}
}
