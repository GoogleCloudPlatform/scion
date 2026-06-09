package main

import (
	"math"
	"os"
	"strings"
	"testing"
)

func TestParseScale(t *testing.T) {
	tests := []struct {
		input   string
		want    Scale
		wantErr bool
	}{
		{"celsius", Celsius, false},
		{"Celsius", Celsius, false},
		{"C", Celsius, false},
		{"fahrenheit", Fahrenheit, false},
		{"F", Fahrenheit, false},
		{"kelvin", Kelvin, false},
		{"K", Kelvin, false},
		{"rankine", 0, true},
		{"", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := parseScale(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseScale(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("parseScale(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func almostEqual(a, b float64) bool {
	return math.Abs(a-b) < 0.01
}

func TestConvert(t *testing.T) {
	tests := []struct {
		name    string
		value   float64
		from    Scale
		to      Scale
		want    float64
		wantErr bool
	}{
		{"C to F: boiling", 100, Celsius, Fahrenheit, 212, false},
		{"F to C: freezing", 32, Fahrenheit, Celsius, 0, false},
		{"C to K: zero", 0, Celsius, Kelvin, 273.15, false},
		{"K to F: boiling", 373.15, Kelvin, Fahrenheit, 212, false},
		{"C to C: identity", 42, Celsius, Celsius, 42, false},
		{"F to F: identity", 100, Fahrenheit, Fahrenheit, 100, false},
		{"K to K: identity", 300, Kelvin, Kelvin, 300, false},
		{"C to F: negative forty", -40, Celsius, Fahrenheit, -40, false},
		{"F to C: body temp", 98.6, Fahrenheit, Celsius, 37, false},
		{"F to C: 100F", 100, Fahrenheit, Celsius, 37.78, false},
		{"K to C: absolute zero", 0, Kelvin, Celsius, -273.15, false},
		{"below absolute zero K", -1, Kelvin, Celsius, 0, true},
		{"below absolute zero C", -274, Celsius, Fahrenheit, 0, true},
		{"below absolute zero F", -460, Fahrenheit, Kelvin, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := convert(tt.value, tt.from, tt.to)
			if (err != nil) != tt.wantErr {
				t.Fatalf("convert(%v, %v, %v) error = %v, wantErr %v", tt.value, tt.from, tt.to, err, tt.wantErr)
			}
			if !tt.wantErr && !almostEqual(got, tt.want) {
				t.Errorf("convert(%v, %v, %v) = %v, want %v", tt.value, tt.from, tt.to, got, tt.want)
			}
		})
	}
}

func runCLI(t *testing.T, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	outFile, _ := os.CreateTemp(t.TempDir(), "stdout")
	errFile, _ := os.CreateTemp(t.TempDir(), "stderr")
	defer outFile.Close()
	defer errFile.Close()

	exitCode = run(args, outFile, errFile)

	outFile.Seek(0, 0)
	outBytes, _ := os.ReadFile(outFile.Name())
	errFile.Seek(0, 0)
	errBytes, _ := os.ReadFile(errFile.Name())

	return string(outBytes), string(errBytes), exitCode
}

func TestCLI_AC007_UnknownScale(t *testing.T) {
	stdout, stderr, code := runCLI(t, "--from", "rankine", "--to", "celsius", "100")
	if code == 0 {
		t.Fatal("expected non-zero exit code for unknown scale")
	}
	if !strings.Contains(stderr, "unknown temperature scale") {
		t.Errorf("expected error about unknown scale, got stderr: %q, stdout: %q", stderr, stdout)
	}
}

func TestCLI_AC008_MissingValue(t *testing.T) {
	_, stderr, code := runCLI(t, "--from", "celsius", "--to", "fahrenheit")
	if code == 0 {
		t.Fatal("expected non-zero exit code for missing value")
	}
	if !strings.Contains(stderr, "usage:") {
		t.Errorf("expected usage message, got stderr: %q", stderr)
	}
}

func TestCLI_AC009_NegativeValue(t *testing.T) {
	stdout, stderr, code := runCLI(t, "--from", "celsius", "--to", "fahrenheit", "-40")
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d; stderr: %q", code, stderr)
	}
	if strings.TrimSpace(stdout) != "-40.00" {
		t.Errorf("expected -40.00, got %q", strings.TrimSpace(stdout))
	}
}

func TestCLI_NaN(t *testing.T) {
	_, stderr, code := runCLI(t, "--from", "celsius", "--to", "fahrenheit", "NaN")
	if code == 0 {
		t.Fatal("expected non-zero exit code for NaN input")
	}
	if !strings.Contains(stderr, "invalid temperature value") {
		t.Errorf("expected invalid value error, got stderr: %q", stderr)
	}
}

func TestCLI_Inf(t *testing.T) {
	_, stderr, code := runCLI(t, "--from", "celsius", "--to", "fahrenheit", "Inf")
	if code == 0 {
		t.Fatal("expected non-zero exit code for Inf input")
	}
	if !strings.Contains(stderr, "invalid temperature value") {
		t.Errorf("expected invalid value error, got stderr: %q", stderr)
	}
}

func TestCLI_NegativeInf(t *testing.T) {
	_, stderr, code := runCLI(t, "--from", "celsius", "--to", "fahrenheit", "-Inf")
	if code == 0 {
		t.Fatal("expected non-zero exit code for -Inf input")
	}
	if !strings.Contains(stderr, "invalid temperature value") {
		t.Errorf("expected invalid value error, got stderr: %q", stderr)
	}
}

func TestCLI_AC006_BelowAbsoluteZero(t *testing.T) {
	_, stderr, code := runCLI(t, "--from", "kelvin", "--to", "celsius", "-1")
	if code == 0 {
		t.Fatal("expected non-zero exit code for below absolute zero")
	}
	if !strings.Contains(stderr, "below absolute zero") {
		t.Errorf("expected absolute zero error, got stderr: %q", stderr)
	}
}

func TestCLI_AC001_CelsiusToFahrenheit(t *testing.T) {
	stdout, stderr, code := runCLI(t, "--from", "celsius", "--to", "fahrenheit", "100")
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d; stderr: %q", code, stderr)
	}
	if strings.TrimSpace(stdout) != "212.00" {
		t.Errorf("expected 212.00, got %q", strings.TrimSpace(stdout))
	}
}
