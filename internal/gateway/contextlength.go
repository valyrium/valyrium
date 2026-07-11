package gateway

import "strings"

// defaultContextLength is used when a model id matches no known Claude
// family and no override was configured.
const defaultContextLength = 200000

// knownContextLengths maps recognizable Claude model-family substrings to
// their context window in tokens. Matched by substring since configured
// model ids vary in form (e.g. "sonnet", "claude-sonnet-4-5-20250929").
var knownContextLengths = []struct {
	substr string
	length int
}{
	{"haiku", 200000},
	{"sonnet", 200000},
	{"opus", 200000},
}

// resolveContextLength returns the context window for a model id: an exact
// override wins, then a known Claude family match, then the configured
// default, then the package default.
func resolveContextLength(id string, overrides map[string]int, defaultLength int) int {
	if l, ok := overrides[id]; ok {
		return l
	}
	lower := strings.ToLower(id)
	for _, k := range knownContextLengths {
		if strings.Contains(lower, k.substr) {
			return k.length
		}
	}
	if defaultLength > 0 {
		return defaultLength
	}
	return defaultContextLength
}
