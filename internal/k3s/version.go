package k3s

import (
	"cmp"
	"fmt"
	"regexp"
	"strconv"
)

// Release is a parsed K3s release tag, e.g. "v1.33.4+k3s1".
type Release struct {
	Major, Minor, Patch int
	// RC is the release candidate number, or 0 for a final release.
	RC int
	// K3s is the K3s revision of the upstream Kubernetes release.
	K3s int
}

// releaseTag matches K3s release tags; the "+k3sN" suffix is required to
// download a release.
var releaseTag = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)(?:-rc(\d+))?\+k3s(\d+)$`)

// ParseRelease parses a full K3s release tag.
func ParseRelease(tag string) (Release, error) {
	m := releaseTag.FindStringSubmatch(tag)
	if m == nil {
		return Release{}, fmt.Errorf("%q isn't a K3s release; use the full release tag, e.g. v1.33.4+k3s1 (see https://github.com/k3s-io/k3s/releases)", tag)
	}
	n := make([]int, len(m))
	for i, s := range m[1:] {
		if s != "" {
			var err error
			if n[i+1], err = strconv.Atoi(s); err != nil {
				return Release{}, fmt.Errorf("%q isn't a K3s release: %w", tag, err)
			}
		}
	}
	return Release{Major: n[1], Minor: n[2], Patch: n[3], RC: n[4], K3s: n[5]}, nil
}

// Compare orders releases, returning -1, 0 or +1. A release candidate
// comes before the final release it precedes.
func (r Release) Compare(o Release) int {
	if c := cmp.Compare(r.Major, o.Major); c != 0 {
		return c
	}
	if c := cmp.Compare(r.Minor, o.Minor); c != 0 {
		return c
	}
	if c := cmp.Compare(r.Patch, o.Patch); c != 0 {
		return c
	}
	if r.RC != o.RC {
		switch {
		case r.RC == 0:
			return 1
		case o.RC == 0:
			return -1
		}
		return cmp.Compare(r.RC, o.RC)
	}
	return cmp.Compare(r.K3s, o.K3s)
}

// CheckUpgrade reports whether a node running K3s release from needs
// upgrading to release to, and refuses moves Kubernetes doesn't support:
// downgrades, and skipping a minor version.
func CheckUpgrade(from, to string) (bool, error) {
	f, err := ParseRelease(from)
	if err != nil {
		return false, fmt.Errorf("installed version: %w", err)
	}
	t, err := ParseRelease(to)
	if err != nil {
		return false, err
	}
	switch c := f.Compare(t); {
	case c == 0:
		return false, nil
	case c > 0:
		return false, fmt.Errorf("K3s %s is installed, which is newer than %s; Kubernetes doesn't support downgrades", from, to)
	case f.Major != t.Major || t.Minor > f.Minor+1:
		return false, fmt.Errorf("upgrading K3s %s to %s would skip a minor version, which Kubernetes doesn't support; upgrade to a v%d.%d release first",
			from, to, f.Major, f.Minor+1)
	}
	return true, nil
}
