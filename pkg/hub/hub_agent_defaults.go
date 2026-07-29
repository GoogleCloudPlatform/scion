package hub

import (
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
)

// hubAgentDefaults returns the hub's operational agent_defaults section.
//
// Thread-safe: ApplySnapshot writes s.config.AgentDefaults under s.mu from the
// settings-propagation goroutine (operational_settings.go, refreshAndApply)
// while request paths read it, so every read must take the lock. Mirrors
// (*Server).HubName, which is the same shape for the same reason.
//
// The returned value is a deep copy — DefaultResources is a pointer, and
// handing callers the live pointer would let a downstream merge mutate the
// server's config out from under the lock.
//
// In file mode this always returns the zero value: BuildLayer1SnapshotFromFile
// deliberately leaves the agent-defaults fields empty (design §3.2.4), so
// callers that gate on "non-empty" never fire in file mode. That is what keeps
// file-mode dispatch byte-identical to the pre-change behaviour.
func (s *Server) hubAgentDefaults() opsettings.AgentDefaultsSettings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d := s.config.AgentDefaults
	if d.DefaultResources != nil {
		rs := *d.DefaultResources
		d.DefaultResources = &rs
	}
	return d
}

// agentDefaultsEqual reports whether two agent_defaults sections carry the same
// values, comparing DefaultResources by pointee rather than by pointer. Used by
// ApplySnapshot to decide whether "agent_defaults" belongs in the applied-fields
// list; a pointer comparison would report a change on every refresh, because
// Snapshot() builds a fresh *api.ResourceSpec each time.
func agentDefaultsEqual(a, b opsettings.AgentDefaultsSettings) bool {
	if a.DefaultTemplate != b.DefaultTemplate ||
		a.DefaultHarnessConfig != b.DefaultHarnessConfig ||
		a.DefaultMaxTurns != b.DefaultMaxTurns ||
		a.DefaultMaxModelCalls != b.DefaultMaxModelCalls ||
		a.DefaultMaxDuration != b.DefaultMaxDuration {
		return false
	}
	return resourceSpecEqual(a.DefaultResources, b.DefaultResources)
}

// resourceSpecEqual compares two resource specs by value, treating nil and a
// zero-valued spec as different (nil means "unset").
func resourceSpecEqual(a, b *api.ResourceSpec) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
