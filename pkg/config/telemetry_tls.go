/*
Copyright 2026 The Scion Authors.
*/

package config

import (
	"fmt"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/util"
)

// ValidateTelemetryTLS rejects contradictory telemetry.cloud.tls settings:
// plaintext together with verification or client certificate options, and
// a client certificate without its key (or the reverse). It checks values
// only: cert_file and key_file are paths inside the agent container, so
// whether they can be read is checked where they live, by the exporter. A
// nil tls is valid.
func ValidateTelemetryTLS(tls *api.TelemetryTLS) error {
	if tls == nil {
		return nil
	}
	clientCert := tls.CertFile != "" || tls.KeyFile != ""
	if tls.Enabled != nil && !*tls.Enabled && ((tls.InsecureSkipVerify != nil && *tls.InsecureSkipVerify) || tls.CAFile != "" || clientCert) {
		return fmt.Errorf("telemetry.cloud.tls: plaintext conflicts with certificate verification or client certificate settings")
	}
	if (tls.CertFile == "") != (tls.KeyFile == "") {
		return fmt.Errorf("telemetry.cloud.tls: cert_file and key_file must be set together")
	}
	return nil
}

// ValidateV1TelemetryTLS applies ValidateTelemetryTLS to a settings-file
// (or hub telemetry section) telemetry.cloud.tls value. It is the one place
// the V1 fields are copied to api.TelemetryTLS. A nil tls is valid.
func ValidateV1TelemetryTLS(tls *V1TelemetryTLSConfig) error {
	if tls == nil {
		return nil
	}
	return ValidateTelemetryTLS(&api.TelemetryTLS{
		Enabled:            tls.Enabled,
		InsecureSkipVerify: tls.InsecureSkipVerify,
		CAFile:             tls.CAFile,
		CertFile:           tls.CertFile,
		KeyFile:            tls.KeyFile,
	})
}

// ValidateTelemetryEnv applies ValidateTelemetryTLS to the OTLP TLS
// variables in a final agent environment (KEY=VALUE entries; the last entry
// for a key wins). This is the result of every merge (settings, template,
// harness config, profile, hub defaults and explicit env), read the way
// sciontool reads it: a boolean that does not parse is ignored.
func ValidateTelemetryEnv(env []string) error {
	vars := map[string]string{}
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		switch key {
		case "SCION_OTEL_INSECURE", "SCION_OTEL_SKIP_TLS_VERIFY",
			"SCION_OTEL_CA_FILE", "SCION_OTEL_CERT_FILE", "SCION_OTEL_KEY_FILE":
			vars[key] = value
		}
	}
	tls := &api.TelemetryTLS{
		CAFile:   vars["SCION_OTEL_CA_FILE"],
		CertFile: vars["SCION_OTEL_CERT_FILE"],
		KeyFile:  vars["SCION_OTEL_KEY_FILE"],
	}
	if plaintext, ok := util.ParseBool(vars["SCION_OTEL_INSECURE"]); ok {
		enabled := !plaintext
		tls.Enabled = &enabled
	}
	if skip, ok := util.ParseBool(vars["SCION_OTEL_SKIP_TLS_VERIFY"]); ok {
		tls.InsecureSkipVerify = &skip
	}
	return ValidateTelemetryTLS(tls)
}
