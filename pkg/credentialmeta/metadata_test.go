package credentialmeta

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateIssuancePreservesExistingMetadataBehavior(t *testing.T) {
	t.Parallel()

	if err := ValidateIssuance("", "  ", map[string]string{"agent_delegation_code": "safe value"}); err != nil {
		t.Fatalf("ValidateIssuance() rejected legitimate existing metadata: %v", err)
	}

	err := ValidateIssuance("SCION_PAT_canary", "", nil)
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) {
		t.Fatalf("ValidateIssuance() error = %T, want *ValidationError", err)
	}
	if validationErr.Field != "name" || strings.Contains(err.Error(), "SCION_PAT_canary") {
		t.Fatalf("ValidateIssuance() error = %q, want stable value-free name error", err)
	}
}

func TestNewRefRejectsEveryUnsafeSerializedField(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*RefInput)
		field  string
	}{
		{"kind enum", func(in *RefInput) { in.Kind = Kind("SCION_PAT_kind") }, "kind"},
		{"id secret", func(in *RefInput) { in.ID = "prefix-sCiOn_PaT_id" }, "id"},
		{"name format", func(in *RefInput) { in.Name = "safe\u200bname" }, "name"},
		{"boundary kind enum", func(in *RefInput) { in.BoundaryKind = BoundaryKind("Bearer canary") }, "boundary_kind"},
		{"boundary project secret", func(in *RefInput) { in.BoundaryProjectID = "Bearer canary" }, "boundary_project_id"},
		{"label key secret", func(in *RefInput) { in.Labels = map[string]string{"scion_pat_key": "safe"} }, "labels"},
		{"label value secret", func(in *RefInput) { in.Labels = map[string]string{"purpose": "BEARER canary"} }, "labels"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			input := RefInput{Kind: KindUAT, ID: "token-1", BoundaryKind: BoundaryProject, BoundaryProjectID: "project-1"}
			tc.mutate(&input)
			_, err := NewRef(input)
			var validationErr *ValidationError
			if !errors.As(err, &validationErr) {
				t.Fatalf("NewRef() error = %T, want *ValidationError", err)
			}
			if validationErr.Field != tc.field {
				t.Fatalf("NewRef() field = %q, want %q", validationErr.Field, tc.field)
			}
			if strings.Contains(strings.ToLower(err.Error()), "canary") || strings.Contains(strings.ToLower(err.Error()), "scion_pat") {
				t.Fatalf("NewRef() reflected rejected value: %q", err)
			}
		})
	}
}

func TestNewRefRejectsControlOrFormatRunesInEverySerializedString(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*RefInput)
	}{
		{"kind", func(in *RefInput) { in.Kind = Kind("uat\u200b") }},
		{"id", func(in *RefInput) { in.ID = "token\n1" }},
		{"name", func(in *RefInput) { in.Name = "deploy\u2028name" }},
		{"boundary kind", func(in *RefInput) { in.BoundaryKind = BoundaryKind("project\u200b") }},
		{"boundary project id", func(in *RefInput) { in.BoundaryProjectID = "project\u2029one" }},
		{"label key", func(in *RefInput) { in.Labels = map[string]string{"pur\npose": "safe"} }},
		{"label value", func(in *RefInput) { in.Labels = map[string]string{"purpose": "safe\u200bvalue"} }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			input := RefInput{Kind: KindUAT, BoundaryKind: BoundaryProject, BoundaryProjectID: "project-1"}
			tc.mutate(&input)
			if _, err := NewRef(input); err == nil {
				t.Fatal("NewRef() accepted a control or format rune")
			}
		})
	}
}

func TestNewRefBoundaryPairMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		kind        BoundaryKind
		projectID   string
		wantAllowed bool
	}{
		{"absent pair", "", "", true},
		{"project with ID", BoundaryProject, "project-1", true},
		{"hub without ID", BoundaryHub, "", true},
		{"project without ID", BoundaryProject, "", false},
		{"hub with ID", BoundaryHub, "project-1", false},
		{"absent kind with ID", "", "project-1", false},
		{"unknown kind", BoundaryKind("unknown"), "", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewRef(RefInput{Kind: KindUAT, BoundaryKind: tc.kind, BoundaryProjectID: tc.projectID})
			if tc.wantAllowed && err != nil {
				t.Fatalf("NewRef() rejected valid boundary pair: %v", err)
			}
			if !tc.wantAllowed && err == nil {
				t.Fatal("NewRef() accepted inconsistent boundary pair")
			}
		})
	}
}

func TestValidationErrorTextIsStableAndValueFree(t *testing.T) {
	t.Parallel()

	_, err := NewRef(RefInput{Kind: KindUAT, ID: "SCION_PAT_error-text-canary"})
	if err == nil {
		t.Fatal("NewRef() accepted secret-shaped ID")
	}
	const want = "invalid credential metadata field id: must not resemble a bearer token or credential value"
	if err.Error() != want {
		t.Fatalf("NewRef() error = %q, want %q", err, want)
	}
}

func TestRefDefensivelyCopiesLabels(t *testing.T) {
	t.Parallel()

	labels := map[string]string{"purpose": "automation"}
	ref, err := NewRef(RefInput{Kind: KindUAT, Labels: labels})
	if err != nil {
		t.Fatalf("NewRef() error: %v", err)
	}
	labels["purpose"] = "changed"
	got := ref.Labels()
	got["purpose"] = "also changed"
	if ref.Labels()["purpose"] != "automation" {
		t.Fatal("Ref labels must not retain or expose mutable aliases")
	}
}

func TestContractConstantsPinServerMetadataBounds(t *testing.T) {
	t.Parallel()

	if MaxNameBytes != 128 || MaxPurposeBytes != 128 || MaxLabelCount != 8 || MaxLabelKeyBytes != 32 || MaxLabelValueBytes != 64 {
		t.Fatalf("credential metadata bounds drifted: name=%d purpose=%d count=%d key=%d value=%d",
			MaxNameBytes, MaxPurposeBytes, MaxLabelCount, MaxLabelKeyBytes, MaxLabelValueBytes)
	}
}
