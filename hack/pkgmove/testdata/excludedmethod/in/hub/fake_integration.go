//go:build integration

package hub

type fake struct{}

func (fake) run() {}

// FakeRuns is true under the integration tag.
var FakeRuns = Start(fake{})
