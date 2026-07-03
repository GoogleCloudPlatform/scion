package slack

import (
	"net/http"

	slackapi "github.com/slack-go/slack"
)

// verifyRequest validates that an inbound HTTP request was signed by Slack
// using the configured signing secret.
func verifyRequest(r *http.Request, body []byte, signingSecret string) error {
	sv, err := slackapi.NewSecretsVerifier(r.Header, signingSecret)
	if err != nil {
		return err
	}
	if _, err := sv.Write(body); err != nil {
		return err
	}
	return sv.Ensure()
}
