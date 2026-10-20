// Package release picks releases: version tags (v1.2.3, v1.2.3-rc.1), compared as SemVer.
package release

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/lsariol/lighthouse/internal/github"
)

// Version is a version tag: the tag as written, its numbers, and its
// pre-release part ("rc.1"; "" for a stable release).
type Version struct {
	Tag                 string
	Major, Minor, Patch int
	Pre                 string
}

var version = regexp.MustCompile(`^v?(0|[1-9]\d{0,8})\.(0|[1-9]\d{0,8})\.(0|[1-9]\d{0,8})(?:-((?:0|[1-9]\d*|\d*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9]\d*|\d*[A-Za-z-][0-9A-Za-z-]*))*))?$`)

// ParseTag reads a version tag, stable or pre-release; ok is false if it
// isn't one.
func ParseTag(tag string) (Version, bool) {
	m := version.FindStringSubmatch(tag)
	if m == nil {
		return Version{}, false
	}
	v := Version{Tag: tag, Pre: m[4]}
	v.Major, _ = strconv.Atoi(m[1])
	v.Minor, _ = strconv.Atoi(m[2])
	v.Patch, _ = strconv.Atoi(m[3])
	return v, true
}

// Parse reads a stable release tag; ok is false for anything else,
// pre-releases included.
func Parse(tag string) (Version, bool) {
	v, ok := ParseTag(tag)
	if !ok || v.Prerelease() {
		return Version{}, false
	}
	return v, true
}

// Prerelease reports whether v is a pre-release, such as v1.2.0-rc.1.
func (v Version) Prerelease() bool { return v.Pre != "" }

// Compare orders versions by SemVer precedence: -1 if v is earlier than w,
// 0 if they're equal, 1 if v is later. A pre-release comes before its release.
func (v Version) Compare(w Version) int {
	for _, d := range [][2]int{{v.Major, w.Major}, {v.Minor, w.Minor}, {v.Patch, w.Patch}} {
		if d[0] != d[1] {
			return cmp(d[0], d[1])
		}
	}
	switch {
	case v.Pre == w.Pre:
		return 0
	case v.Pre == "":
		return 1
	case w.Pre == "":
		return -1
	}
	a, b := strings.Split(v.Pre, "."), strings.Split(w.Pre, ".")
	for i := 0; i < len(a) && i < len(b); i++ {
		if c := comparePart(a[i], b[i]); c != 0 {
			return c
		}
	}
	return cmp(len(a), len(b))
}

// comparePart compares pre-release identifiers: numbers as numbers, below
// anything with letters, which compare as text.
func comparePart(a, b string) int {
	na, errA := strconv.Atoi(a)
	nb, errB := strconv.Atoi(b)
	switch {
	case errA == nil && errB == nil:
		return cmp(na, nb)
	case errA == nil:
		return -1
	case errB == nil:
		return 1
	}
	return strings.Compare(a, b)
}

func cmp(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Newer reports whether v is a later version than w.
func (v Version) Newer(w Version) bool { return v.Compare(w) > 0 }

// Newest returns the newest stable release among tags.
func Newest(tags []github.Tag) (github.Tag, Version, bool) {
	var best github.Tag
	var bestV Version
	found := false
	for _, t := range tags {
		v, ok := Parse(t.Name)
		if ok && (!found || v.Newer(bestV)) {
			best, bestV, found = t, v, true
		}
	}
	return best, bestV, found
}

// Higher returns whichever of two versions is the higher stable release: a,
// b, or, when neither is one, "".
func Higher(a string, b string) string {
	va, okA := Parse(a)
	vb, okB := Parse(b)
	switch {
	case okA && okB && vb.Newer(va), !okA && okB:
		return b
	case okA:
		return a
	}
	return ""
}

// Find returns the tag called name.
func Find(tags []github.Tag, name string) (github.Tag, bool) {
	for _, t := range tags {
		if t.Name == name {
			return t, true
		}
	}
	return github.Tag{}, false
}
