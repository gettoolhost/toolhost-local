// Package namespace owns the qualified-tool-name grammar: backend + "__" +
// tool. Ported from the reference tree (internal/namespace) — the separator
// constraints it encodes were learned against real clients and are kept
// verbatim.
package namespace

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Separator joins a backend namespace and a tool name into one wire-visible
// identifier ("github__search_repos"). Constraints:
//
//  1. Every character must be valid inside an MCP tool `name` field —
//     clients (notably Claude Desktop's connector validation) enforce
//     ^[a-zA-Z0-9_-]{1,64}$ on the composed name. "::" uses ':' which is
//     outside that charset; "." and "/" are likewise rejected by strict
//     clients.
//  2. The only wire-legal characters are also legitimately used inside
//     backend namespaces and tool names ("openai_docs", "deepwiki-
//     knowledge"), so no single character is an unambiguous delimiter —
//     the two-character "__" keeps the reserved-separator property.
const Separator = "__"

// MaxNamespacedNameLength is the wire ceiling from the client regex above.
const MaxNamespacedNameLength = 64

// ErrNameTooLong distinguishes "this one combination is too long" from
// malformed names — match with errors.Is, never on the error string.
var ErrNameTooLong = errors.New("namespaced name exceeds the MCP client-compatible length ceiling")

// ErrNameCharset distinguishes "this upstream name has a wire-illegal
// character" — a per-tool skip, never a whole-backend failure.
var ErrNameCharset = errors.New("name contains characters outside the MCP client-compatible charset")

var nameCharsetPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

func Join(namespace string, name string) (string, error) {
	if err := ValidateNamespace(namespace); err != nil {
		return "", err
	}
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("name must not be empty")
	}
	if strings.Contains(name, Separator) {
		return "", fmt.Errorf("name must not contain %q", Separator)
	}
	if !nameCharsetPattern.MatchString(name) {
		return "", fmt.Errorf("name %q contains characters outside the MCP client-compatible charset %s: %w",
			name, nameCharsetPattern.String(), ErrNameCharset)
	}

	joined := namespace + Separator + name
	if len(joined) > MaxNamespacedNameLength {
		return "", fmt.Errorf("namespaced name %q is %d characters, exceeds the %d-character MCP "+
			"client-compatible ceiling (namespace %q + name %q): %w",
			joined, len(joined), MaxNamespacedNameLength, namespace, name, ErrNameTooLong)
	}

	return joined, nil
}

func Split(namespacedName string) (string, string, error) {
	parts := strings.Split(namespacedName, Separator)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("namespaced name must contain exactly one %q", Separator)
	}
	if err := ValidateNamespace(parts[0]); err != nil {
		return "", "", err
	}
	if strings.TrimSpace(parts[1]) == "" {
		return "", "", fmt.Errorf("name must not be empty")
	}
	return parts[0], parts[1], nil
}

func ValidateNamespace(value string) error {
	if value == "" {
		return fmt.Errorf("namespace must not be empty")
	}
	if strings.Contains(value, Separator) {
		return fmt.Errorf("namespace must not contain %q", Separator)
	}
	// A namespace ending in "_" combined with a name starting with "_"
	// collides byte-for-byte: Join("a_", "b") and Join("a", "_b") both
	// produce "a___b". Rejecting the trailing underscore closes it.
	if strings.HasSuffix(value, "_") {
		return fmt.Errorf("namespace must not end with %q (would collide with a leading %q on the name)", "_", "_")
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return fmt.Errorf("namespace must contain only lowercase letters, numbers, underscore, or dash")
	}
	return nil
}
