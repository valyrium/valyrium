package gateway

import (
	"strings"
	"testing"
)

func TestNewCompletionIDIsUnique(t *testing.T) {
	const n = 1000
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		id := newCompletionID()
		if !strings.HasPrefix(id, "chatcmpl-") {
			t.Fatalf("expected id to have chatcmpl- prefix, got %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate completion id generated: %q", id)
		}
		seen[id] = true
	}
}
