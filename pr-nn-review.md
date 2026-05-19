## Review Summary

**Verdict:** APPROVE

**Overview:** The changes successfully address the "grove" to "project" rename by providing robust JSON marshaling/unmarshaling for Hub response types and heartbeats. The implementation correctly handles legacy fields and avoids shadowing issues using established Go patterns.

### Critical Issues
- None

### Important Issues
- None

### Suggestions

- **File: pkg/hub/response_types.go:42, 87, 115, ...**
  **Consistency in `omitempty` for legacy fields:** Legacy fields such as `groveId`, `groveName`, and `grove` are marked `omitempty` in some types (e.g., `TemplateWithCapabilities`, `GroupWithCapabilities`) but are mandatory in others (e.g., `AgentWithCapabilities`, `ProjectWithCapabilities`). 
  *Suggested Fix:* Use `omitempty` consistently for all legacy fields to ensure that if for some reason the source field is empty, we don't send an explicit empty string for the legacy key.

- **File: pkg/hub/response_types.go:50, 94**
  **Performance (Double Unmarshaling):** `AgentWithCapabilities` and `ProjectWithCapabilities` unmarshal the same JSON input twice. While this correctly leverages the embedded model's `UnmarshalJSON`, it is slightly less efficient than a single-pass approach.
  *Suggested Fix:* This is acceptable for readability, but consider if the volume of these requests justifies optimizing into a single pass using a combined alias struct.

### What's Done Well
- **Correct Pattern for Embedded Marshaling:** The use of `type Alias T` to bypass the embedded type's `MarshalJSON`/`UnmarshalJSON` methods is exactly the right way to avoid infinite recursion and ensure wrapper fields are included.
- **Robust Heartbeat Support:** The Hub's heartbeat handler correctly supports both the new `projects` key and the legacy `groves` key, ensuring older brokers continue to work during the transition.
- **Comprehensive Legacy Coverage:** The PR goes beyond just renaming and ensures that all relevant API entities (Policies, Groups, Templates) maintain backward compatibility.
- **Solid Test Suite:** The addition of `heartbeat_legacy_test.go` and updates to `runtime_brokers_test.go` provide good confidence in the bidirectional compatibility.

### Verification Story
- Tests reviewed: Yes. `TestBrokerHeartbeatRequest_UnmarshalJSON`, `TestBrokerProjectHeartbeat_UnmarshalJSON`, and `ListBrokerProjectsResponse` tests verify the legacy mapping.
- Build verified: Yes. `go build ./...` passes.
- Lint/static analysis clean: Yes. The code follows standard Go idioms for JSON handling.
- Security checked: Yes. No unsanitized inputs or insecure handling identified in these JSON transformations.
