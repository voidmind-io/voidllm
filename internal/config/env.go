package config

import (
	"os"
	"regexp"
)

// envVarRe matches ${VAR} and ${VAR:-fallback} patterns in configuration files.
// group 1: variable name, group 2: optional fallback value after :-
var envVarRe = regexp.MustCompile(`\$\{([^}:]+)(?::-(.*?))?\}`)

// interpolateEnv replaces all ${VAR} and ${VAR:-fallback} references in data
// with the corresponding environment variable values. If the variable is unset
// or empty and a fallback is provided, the fallback is used. If no fallback is
// provided and the variable is unset, the reference is replaced with an empty string.
func interpolateEnv(data []byte) []byte {
	return envVarRe.ReplaceAllFunc(data, func(match []byte) []byte {
		parts := envVarRe.FindSubmatch(match)
		// parts[1] is the variable name, parts[2] is the optional fallback.
		name := string(parts[1])
		fallback := string(parts[2])

		if val := os.Getenv(name); val != "" {
			return []byte(val)
		}
		return []byte(fallback)
	})
}
