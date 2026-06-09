# Release Notes (2026-06-09)

## [0.0.1.0] - 2026-06-09

### Added
- New `tempconv` CLI tool for temperature conversion between Celsius, Fahrenheit, and Kelvin. Usage: `tempconv --from celsius --to fahrenheit 100` outputs `212.00`
- Validates against absolute zero and rejects physically impossible temperatures
- Handles negative values (e.g., `-40`) and rejects NaN/Inf inputs
- Supports short-form scale aliases (`c`, `f`, `k`) in addition to full names
- 22 tests covering all 10 acceptance criteria at both unit and CLI integration levels
