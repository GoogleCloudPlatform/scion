package hub

import "strconv"

type keyer interface {
	// APIKey returns the credential.
	APIKey() string
}

type envKeyer struct{}

// APIKey returns a fixed key; the override renames every implementer.
func (envKeyer) APIKey() string { return "env" }

// Describe uses the moved code.
func Describe(kind string, k keyer) string {
	return httpStatusText(kind) + ":" + k.APIKey() + ":" + strconv.Itoa(httpStatus(kind))
}

// Client returns the moved client as a keyer.
func Client() keyer { return newClient("k") }
