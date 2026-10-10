package slack

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// staleAccountLinkText is shown when the hub no longer accepts the Slack
// user's linked Scion account (the account was removed or deactivated).
const staleAccountLinkText = "Your linked Scion account is no longer active. Run `/scion unregister`, then `/scion register`."

// missingEmailLinkText is shown when the Slack user's link has no Scion
// account email, so requests cannot be made as that user.
const missingEmailLinkText = "Your Slack link has no Scion account email. Run `/scion unregister`, then `/scion register`."

// isStaleAccountLink reports whether a denied request was rejected because
// the linked Scion account is unknown to the hub or no longer active.
func (e *hubError) isStaleAccountLink() bool {
	if e == nil || e.StatusCode != http.StatusForbidden {
		return false
	}
	msg := strings.ToLower(e.Message)
	for _, marker := range []string{
		// Request with the linked user.
		"on-behalf-of principal not found",
		"on-behalf-of principal is not active",
		// Inbound message from the linked user.
		"sender identity could not be resolved",
		"sender identity is not active",
	} {
		if strings.HasPrefix(msg, marker) {
			return true
		}
	}
	return false
}

// deniedRequestText returns actionable text for a hub read the linked user
// was denied, or "" when err is not a denied request. email is the linked
// Scion account's email; project is the project name shown to the user, or
// "" when the request is not about a single project.
func deniedRequestText(err error, email, project string) string {
	var he *hubError
	if !errors.As(err, &he) || he.StatusCode != http.StatusForbidden {
		return ""
	}
	if he.isStaleAccountLink() {
		return staleAccountLinkText
	}
	action := deniedActionPhrase(he.DeniedAction, he.ResourceType)
	where := ""
	if project != "" {
		where = " in " + project
	}
	return fmt.Sprintf("Your Scion account (%s) doesn't have permission to %s%s. Ask a project owner.", email, action, where)
}

// deniedActionPhrase turns a denied action and resource type into a short
// phrase, e.g. ("list", "agent") -> "list agents".
func deniedActionPhrase(action, resourceType string) string {
	if action == "" {
		return "do that"
	}
	if resourceType == "" {
		return action
	}
	return action + " " + pluralNoun(resourceType)
}

// pluralNoun returns the plural of a resource type noun, e.g. "agent" ->
// "agents", "policy" -> "policies".
func pluralNoun(noun string) string {
	switch {
	case strings.HasSuffix(noun, "y") && len(noun) > 1 && !strings.ContainsRune("aeiou", rune(noun[len(noun)-2])):
		return noun[:len(noun)-1] + "ies"
	case strings.HasSuffix(noun, "s"), strings.HasSuffix(noun, "x"),
		strings.HasSuffix(noun, "ch"), strings.HasSuffix(noun, "sh"):
		return noun + "es"
	default:
		return noun + "s"
	}
}
