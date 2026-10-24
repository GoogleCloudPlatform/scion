package main

import (
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
