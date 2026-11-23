package release

import (
	"cmp"
	"strings"
)

// CsAC versions have a numeric dotted core and an optional numeric -r suffix.
// A final release sorts after all revisions of the same core.
func compareVersions(a, b string) (int, bool) {
	aCore, aRevision, aOK := parseVersion(a)
	bCore, bRevision, bOK := parseVersion(b)
	if !aOK || !bOK {
		return 0, false
	}
	for i := 0; i < max(len(aCore), len(bCore)); i++ {
		var av, bv string
		if i < len(aCore) {
			av = aCore[i]
		}
		if i < len(bCore) {
			bv = bCore[i]
		}
		if n := compareDigits(av, bv); n != 0 {
			return n, true
		}
	}
	if aRevision == "" || bRevision == "" {
		return cmp.Compare(len(bRevision), len(aRevision)), true
	}
	return compareDigits(aRevision, bRevision), true
}

func parseVersion(version string) (core []string, revision string, ok bool) {
	if !safeSegment.MatchString(version) {
		return nil, "", false
	}
	version = stripVersionPrefix(version)
	base, revision, hasRevision := strings.Cut(version, "-r")
	if hasRevision && !digits(revision) {
		return nil, "", false
	}
	core = strings.Split(base, ".")
	for _, n := range core {
		if !digits(n) {
			return nil, "", false
		}
	}
	return core, revision, true
}

func stripVersionPrefix(version string) string {
	if version != "" && (version[0] == 'v' || version[0] == 'V') {
		return version[1:]
	}
	return version
}

func digits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func compareDigits(a, b string) int {
	a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
	if n := cmp.Compare(len(a), len(b)); n != 0 {
		return n
	}
	return strings.Compare(a, b)
}

func newer(version, installed string) bool {
	if installed == "" {
		return true
	}
	n, ok := compareVersions(version, installed)
	return ok && n > 0
}
