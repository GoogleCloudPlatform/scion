# Release Notes (2026-06-09)

## [0.0.1.0] - 2026-06-09

### Added
- New `tempconv` CLI tool for temperature conversion between Celsius, Fahrenheit, and Kelvin
- Hub-and-spoke conversion pattern routing all conversions through Celsius for consistency
- Absolute zero validation that rejects physically impossible temperatures
- Negative value handling via argument preprocessing for Go's flag package
- NaN and Inf input rejection for robust error handling
- Support for short-form scale aliases (c, f, k) in addition to full names
- 22 tests covering all 10 acceptance criteria at both unit and CLI integration levels
