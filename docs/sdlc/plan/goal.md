# Goal: Temperature Converter CLI Tool (tempconv)

## App Type: CLI_TOOL

## Description
Build a temperature converter CLI tool (`tempconv`) in Go that converts between Celsius, Fahrenheit, and Kelvin. The tool accepts input via flags and prints the result.

### Usage
```
tempconv --from celsius --to fahrenheit 100
```

### Supported Scales
- Celsius (C)
- Fahrenheit (F)
- Kelvin (K)

### Conversion Formulas
- C → F: `F = C × 9/5 + 32`
- C → K: `K = C + 273.15`
- F → C: `C = (F - 32) × 5/9`
- F → K: `K = (F - 32) × 5/9 + 273.15`
- K → C: `C = K - 273.15`
- K → F: `F = (K - 273.15) × 9/5 + 32`

## Research Summary
- The host repo is a Go 1.26.1 project using Cobra for CLI commands.
- The tool will be a standalone binary under `cmd/tempconv/`.
- Standard Go `flag` package is sufficient (no need for Cobra for a simple tool).
- Unit tests will use the standard `testing` package with table-driven tests.
- Kelvin has an absolute zero constraint (no value below 0 K / -273.15°C / -459.67°F).
