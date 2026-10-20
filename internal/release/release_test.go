package release

import (
	"testing"

	"github.com/lsariol/lighthouse/internal/github"
)

func TestParse(t *testing.T) {
	for _, tag := range []string{"v1.2.3", "1.2.3", "v0.0.1", "v10.20.30"} {
		if _, ok := Parse(tag); !ok {
			t.Errorf("%s isn't a release", tag)
		}
	}
	for _, tag := range []string{"v1.1.0-rc.1", "latest", "v1.2", "v01.2.3", "release-1.2.3", "v1.2.3+build", ""} {
		if _, ok := Parse(tag); ok {
			t.Errorf("%s is a release", tag)
		}
	}
}

func TestParseTag(t *testing.T) {
	for _, tag := range []string{"v1.2.3", "v1.2.0-rc.1", "v1.0.0-alpha", "v1.0.0-0.3.7", "v1.0.0-x-y.1"} {
		if _, ok := ParseTag(tag); !ok {
			t.Errorf("%s isn't a version tag", tag)
		}
	}
	for _, tag := range []string{"v1.2.0-", "v1.2.0-rc..1", "v1.2.0-01", "v1.2.3+build", "latest"} {
		if _, ok := ParseTag(tag); ok {
			t.Errorf("%s is a version tag", tag)
		}
	}
	if v, _ := ParseTag("v2.0.0-rc.1"); !v.Prerelease() || v.Pre != "rc.1" || v.Major != 2 {
		t.Errorf("ParseTag(v2.0.0-rc.1) = %+v", v)
	}
}

// The order from semver.org §11.
func TestCompare(t *testing.T) {
	order := []string{"v1.0.0-alpha", "v1.0.0-alpha.1", "v1.0.0-alpha.beta", "v1.0.0-beta", "v1.0.0-beta.2",
		"v1.0.0-beta.11", "v1.0.0-rc.1", "v1.0.0", "v1.0.1", "v1.2.0", "v1.10.0", "v2.0.0"}
	for i := 0; i+1 < len(order); i++ {
		a, _ := ParseTag(order[i])
		b, _ := ParseTag(order[i+1])
		if a.Compare(b) != -1 || b.Compare(a) != 1 || !b.Newer(a) {
			t.Errorf("%s should come before %s", order[i], order[i+1])
		}
	}
	if a, _ := ParseTag("v1.0.0"); a.Compare(a) != 0 {
		t.Error("a version isn't equal to itself")
	}
}

func TestHigher(t *testing.T) {
	for _, c := range [][3]string{{"v1.2.0", "v1.10.0", "v1.10.0"}, {"v1.2.0", "v1.0.0", "v1.2.0"}, {"", "v1.0.0", "v1.0.0"},
		{"v1.0.0", "", "v1.0.0"}, {"", "", ""}, {"latest", "v0.1.0", "v0.1.0"}, {"v1.0.0", "v2.0.0-rc.1", "v1.0.0"}} {
		if got := Higher(c[0], c[1]); got != c[2] {
			t.Errorf("Higher(%q, %q) = %q, want %q", c[0], c[1], got, c[2])
		}
	}
}

func TestNewest(t *testing.T) {
	tags := []github.Tag{{Name: "latest", SHA: "x"}, {Name: "v1.9.0", SHA: "a"}, {Name: "v1.10.0", SHA: "b"}, {Name: "v2.0.0-rc.1", SHA: "c"}, {Name: "1.10.0", SHA: "d"}}
	tag, v, ok := Newest(tags)
	if !ok || tag.Name != "v1.10.0" || tag.SHA != "b" || v.Minor != 10 {
		t.Errorf("Newest = %+v, %+v, %v", tag, v, ok)
	}
	if _, _, ok := Newest([]github.Tag{{Name: "latest", SHA: "x"}}); ok {
		t.Error("a release among no releases")
	}
	if tag, ok := Find(tags, "v1.9.0"); !ok || tag.SHA != "a" {
		t.Errorf("Find = %+v, %v", tag, ok)
	}
}
