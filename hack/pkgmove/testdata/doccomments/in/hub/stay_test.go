package hub

import "testing"

func TestRun(t *testing.T) {
	if got, want := Run()+Helper(), "error: run (see writeError)error: x (see writeError)jjobstay3error: helper (see writeError)"; got != want {
		t.Fatalf("Run() = %q, want %q", got, want)
	}
}
