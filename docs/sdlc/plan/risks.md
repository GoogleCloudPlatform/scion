# Risk Register — Hello-World CLI

## P0 (must-fix before shipping)

*None identified.* This is a standalone hello-world tool with no external dependencies, no I/O beyond stdout, no persistence, and no auth.

## P1 (should-fix before shipping)

### R-001: Time zone ambiguity
- **Category:** Integration
- **Risk:** The greeting displays local time, which varies by environment. CI and containers may have different `TZ` settings.
- **Mitigation:** Document that the tool uses the system's local time. Tests use fixed `time.Time` values so they are timezone-independent. Integration tests should assert format (`HH:MM:SS`) rather than specific time values.

### R-002: Module dependency footprint
- **Category:** Performance
- **Risk:** Adding cobra as a dependency for a trivial CLI may feel heavy. However, the project already depends on cobra, so no new dependency is introduced.
- **Mitigation:** No action needed — cobra is already in `go.mod`.
