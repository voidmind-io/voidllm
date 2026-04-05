// Package provider defines the canonical set of supported LLM provider names
// used across config validation and the Admin API.
package provider

import "sort"

// ValidProviders is the canonical set of supported LLM provider names.
// Both config validation and Admin API validation reference this map.
var ValidProviders = map[string]bool{
	"openai":           true,
	"anthropic":        true,
	"azure":            true,
	"gemini":           true,
	"vertex":           true,
	"vllm":             true,
	"ollama":           true,
	"custom":           true,
	"bedrock":          true,
	"bedrock-converse": true,
}

// Names returns the supported provider names as a sorted slice,
// suitable for inclusion in error messages.
func Names() []string {
	names := make([]string, 0, len(ValidProviders))
	for k := range ValidProviders {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
