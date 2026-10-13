package sub

import "net/http"

// HTTPStatus maps an error kind to an HTTP status; see also HttpStatusText.
func HTTPStatus(kind string) int {
	if kind == "missing" {
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}

// HttpStatusText is exported mechanically (no override).
func HttpStatusText(kind string) string { return http.StatusText(HTTPStatus(kind)) }

// APIClient carries an API key.
type APIClient struct{ key string }

// APIKey returns the key; lookups go through keyer.APIKey.
func (c APIClient) APIKey() string { return c.key }

// NewClient builds an APIClient.
func NewClient(key string) APIClient { return APIClient{key: key} }
