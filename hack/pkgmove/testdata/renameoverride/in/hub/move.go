package hub

import "net/http"

// httpStatus maps an error kind to an HTTP status; see also httpStatusText.
func httpStatus(kind string) int {
	if kind == "missing" {
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}

// httpStatusText is exported mechanically (no override).
func httpStatusText(kind string) string { return http.StatusText(httpStatus(kind)) }

// apiClient carries an API key.
type apiClient struct{ key string }

// apiKey returns the key; lookups go through keyer.apiKey.
func (c apiClient) apiKey() string { return c.key }

// newClient builds an apiClient.
func newClient(key string) apiClient { return apiClient{key: key} }
