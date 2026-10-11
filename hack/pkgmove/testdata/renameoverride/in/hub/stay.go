package hub

import "strconv"

type keyer interface {
	// apiKey returns the credential.
	apiKey() string
}

type envKeyer struct{}

// apiKey returns a fixed key; the override renames every implementer.
func (envKeyer) apiKey() string { return "env" }

// Describe uses the moved code.
func Describe(kind string, k keyer) string {
	return httpStatusText(kind) + ":" + k.apiKey() + ":" + strconv.Itoa(httpStatus(kind))
}

// Client returns the moved client as a keyer.
func Client() keyer { return newClient("k") }
