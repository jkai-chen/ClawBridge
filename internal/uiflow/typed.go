package uiflow

import (
	"slices"
	"strings"
)

// TypedMatches is true when typed is exactly want. An empty want never
// matches, and case counts.
func TypedMatches(want, typed string) bool {
	return want != "" && typed == want
}

// TypedListMatches is true when the comma-separated typedCSV, each name
// trimmed of spaces, is exactly want, in order.
func TypedListMatches(want []string, typedCSV string) bool {
	typed := []string{}
	for _, name := range strings.Split(typedCSV, ",") {
		typed = append(typed, strings.TrimSpace(name))
	}
	return slices.Equal(typed, want)
}
