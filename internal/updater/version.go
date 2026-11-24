// Package updater implements built-in self-updating for Gatehouse across
// domestic OCI registries (ACR/CCR) and GitHub Releases, including Ed25519
// verification, graceful in-flight draining, release management, and parent
// process supervision with automatic rollback.
package updater

import (
	"fmt"
	"strconv"
	"strings"
)

// ValidateVersion ensures a release version string is safe as a directory name
// and matches the project release naming convention ([A-Za-z0-9._-]+).
func ValidateVersion(v string) error {
	if v == "" || v == "." || v == ".." {
		return fmt.Errorf("invalid version %q", v)
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		ok := (c >= 'A' && c <= 'Z') ||
			(c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') ||
			c == '.' || c == '_' || c == '-'
		if !ok {
			return fmt.Errorf("invalid version %q: disallowed character %q", v, c)
		}
	}
	return nil
}

type semver struct {
	major int
	minor int
	patch int
	pre   []string
}

func parseSemver(raw string) (semver, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return semver{}, false
	}
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	var pre []string
	if i := strings.IndexByte(s, '-'); i >= 0 {
		prePart := s[i+1:]
		s = s[:i]
		if prePart == "" {
			return semver{}, false
		}
		pre = strings.Split(prePart, ".")
		for _, seg := range pre {
			if seg == "" {
				return semver{}, false
			}
		}
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	maj, err1 := parseNonNegInt(parts[0])
	min, err2 := parseNonNegInt(parts[1])
	pat, err3 := parseNonNegInt(parts[2])
	if err1 != nil || err2 != nil || err3 != nil {
		return semver{}, false
	}
	return semver{major: maj, minor: min, patch: pat, pre: pre}, true
}

func parseNonNegInt(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	if len(s) > 1 && s[0] == '0' {
		return 0, fmt.Errorf("leading zero")
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("bad int")
	}
	return n, nil
}

// CompareVersions compares two SemVer strings (with optional leading "v").
// Returns (-1|0|1, true) when both inputs are valid SemVer, or (0, false) otherwise.
func CompareVersions(a, b string) (int, bool) {
	va, oka := parseSemver(a)
	vb, okb := parseSemver(b)
	if !oka || !okb {
		return 0, false
	}
	if va.major != vb.major {
		return cmpInt(va.major, vb.major), true
	}
	if va.minor != vb.minor {
		return cmpInt(va.minor, vb.minor), true
	}
	if va.patch != vb.patch {
		return cmpInt(va.patch, vb.patch), true
	}
	return cmpPre(va.pre, vb.pre), true
}

func cmpInt(a, b int) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func cmpPre(a, b []string) int {
	switch {
	case len(a) == 0 && len(b) == 0:
		return 0
	case len(a) == 0:
		return 1 // stable > prerelease
	case len(b) == 0:
		return -1
	}
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] == b[i] {
			continue
		}
		ai, errA := parseNonNegInt(a[i])
		bi, errB := parseNonNegInt(b[i])
		switch {
		case errA == nil && errB == nil:
			return cmpInt(ai, bi)
		case errA == nil:
			return -1 // numeric < non-numeric per SemVer 2.0
		case errB == nil:
			return 1
		default:
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return cmpInt(len(a), len(b))
}

// IsNewerVersion reports whether remote represents a newer release than current.
func IsNewerVersion(remote, current string) bool {
	r := strings.TrimSpace(remote)
	c := strings.TrimSpace(current)
	if r == "" {
		return false
	}
	if cmp, ok := CompareVersions(r, c); ok {
		return cmp > 0
	}
	// Dev / unversioned builds always accept a non-empty remote version unless equal.
	if c == "" || c == "dev" || strings.HasSuffix(c, "-dev") {
		return r != c
	}
	return false
}
