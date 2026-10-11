package hub

import "testing"

func TestDescribe(t *testing.T) {
	if got, want := Describe("missing", Client())+"|"+Describe("other", envKeyer{}), "Not Found:k:404|Internal Server Error:env:500"; got != want {
		t.Fatalf("Describe = %q, want %q", got, want)
	}
}
