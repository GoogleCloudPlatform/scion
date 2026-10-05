package slack

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDeniedRequestText(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		project string
		want    string
	}{
		{
			name:    "denied action names the account, action and project",
			err:     &hubError{StatusCode: http.StatusForbidden, Code: "forbidden", Message: "Insufficient permissions", ResourceType: "agent", DeniedAction: "list"},
			project: "proj-one",
			want:    "Your Scion account (alice@example.com) doesn't have permission to list agents in proj-one. Ask a project owner.",
		},
		{
			name: "denied without a project",
			err:  &hubError{StatusCode: http.StatusForbidden, Code: "forbidden", DeniedAction: "list", ResourceType: "project"},
			want: "Your Scion account (alice@example.com) doesn't have permission to list projects. Ask a project owner.",
		},
		{
			name: "linked user not found",
			err:  fmt.Errorf("list agents: %w", &hubError{StatusCode: http.StatusForbidden, Code: "forbidden", Message: "on-behalf-of principal not found"}),
			want: staleAccountLinkText,
		},
		{
			name: "linked user not active",
			err:  &hubError{StatusCode: http.StatusForbidden, Code: "forbidden", Message: "on-behalf-of principal is not active (status: suspended)"},
			want: staleAccountLinkText,
		},
		{
			name: "not a denied request",
			err:  &hubError{StatusCode: http.StatusInternalServerError, Code: "internal_error"},
			want: "",
		},
		{
			name: "transport error",
			err:  errors.New("connection refused"),
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, deniedRequestText(tt.err, "alice@example.com", tt.project))
		})
	}
}

func TestStaleAccountLinkText(t *testing.T) {
	assert.Equal(t, "Your linked Scion account is no longer active. Run `/scion unregister`, then `/scion register`.", staleAccountLinkText)
}

func TestHubErrorUserFacingMessage_InboundDenials(t *testing.T) {
	tests := []struct {
		name string
		he   *hubError
		want string
	}{
		{"sender not found", &hubError{StatusCode: http.StatusForbidden, Code: "forbidden", Message: "sender identity could not be resolved"}, staleAccountLinkText},
		{"sender not active", &hubError{StatusCode: http.StatusForbidden, Code: "forbidden", Message: "sender identity is not active"}, staleAccountLinkText},
		{"linked user not found", &hubError{StatusCode: http.StatusForbidden, Code: "forbidden", Message: "on-behalf-of principal not found"}, staleAccountLinkText},
		{"other forbidden", &hubError{StatusCode: http.StatusForbidden, Code: "forbidden", Message: "Insufficient permissions"}, "You don't have permission to message this agent."},
		{"message denied", &hubError{StatusCode: http.StatusForbidden, Code: "message_denied", Message: "Message delivery denied"}, "You don't have permission to message this agent. Ask a project owner."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.he.userFacingMessage())
		})
	}
}

func TestParseHubError_DeniedDetails(t *testing.T) {
	hub := newFakeHub(t)
	hub.on("GET", "/x", http.StatusForbidden,
		`{"error":{"code":"forbidden","message":"Insufficient permissions","details":{"resource_type":"agent","denied_action":"list"}}}`)
	resp, err := http.Get(hub.server.URL + "/x")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	he := parseHubError(resp)
	assert.Equal(t, http.StatusForbidden, he.StatusCode)
	assert.Equal(t, "forbidden", he.Code)
	assert.Equal(t, "agent", he.ResourceType)
	assert.Equal(t, "list", he.DeniedAction)
}

func TestDeniedActionPhrase(t *testing.T) {
	tests := []struct{ action, resourceType, want string }{
		{"list", "agent", "list agents"},
		{"read", "policy", "read policies"},
		{"read", "gateway", "read gateways"},
		{"list", "access", "list accesses"},
		{"list", "", "list"},
		{"", "agent", "do that"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, deniedActionPhrase(tt.action, tt.resourceType))
	}
}
