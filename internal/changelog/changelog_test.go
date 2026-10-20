package changelog

import (
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The heading styles in use: Lighthouse and SparkDB now, Lighthouse before
// 2026-10-20, and one still undated.
const sample = "# Changelog\r\n\r\nAll notable changes.\r\n\r\n" +
	"## v1.3.0 (2026-10-20)\r\n\r\nShort summary.\r\n\r\n### Added\r\n- **A thing.** Details.\r\n\r\n" +
	"## v1.2.0\r\n\r\n### Fixed\r\n- Undated.\r\n\r\n" +
	"## [1.1.0] — 2026-10-07\r\n\r\n### Changed\r\n- Old style.\r\n\r\n" +
	"## v1.0.0 (2026-09-01)\r\n"

var published = time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)

func TestFind(t *testing.T) {
	s, ok := Find([]byte(sample), "v1.3.0")
	if !ok || s.Date != "2026-10-20" || s.Body != "Short summary.\n\n### Added\n- **A thing.** Details." {
		t.Errorf("v1.3.0 = %+v, %v", s, ok)
	}
	if s, ok := Find([]byte(sample), "v1.1.0"); !ok || s.Date != "2026-10-07" || s.Body != "### Changed\n- Old style." {
		t.Errorf("v1.1.0 = %+v, %v", s, ok)
	}
	if s, ok := Find([]byte(sample), "1.2.0"); !ok || s.Date != "" {
		t.Errorf("1.2.0 = %+v, %v", s, ok)
	}
	if s, ok := Find([]byte(sample), "v1.0.0"); !ok || s.Body != "" {
		t.Errorf("v1.0.0 (last, empty) = %+v, %v", s, ok)
	}
	if _, ok := Find([]byte(sample), "v1.3"); ok {
		t.Error("v1.3 matched v1.3.0")
	}
	if _, ok := Find([]byte("## v1.3.0-rc.1 (2026-10-19)\n"), "v1.3.0"); ok {
		t.Error("v1.3.0 matched v1.3.0-rc.1")
	}
}

// Lighthouse's own CHANGELOG: every released version has a dated section.
func TestOwnChangelog(t *testing.T) {
	text, err := os.ReadFile("../../" + File)
	if errors.Is(err, os.ErrNotExist) {
		t.Skip(File + " isn't here (the Docker build leaves out .md files)")
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range regexp.MustCompile(`(?m)^## (v\d+\.\d+\.\d+) \((\d{4}-\d{2}-\d{2})\)`).FindAllStringSubmatch(string(text), -1) {
		if s, ok := Find(text, m[1]); !ok || s.Date != m[2] || s.Body == "" {
			t.Errorf("%s: %+v, %v", m[1], s, ok)
		}
	}
}

func TestNotes(t *testing.T) {
	cases := []struct {
		text, version, want, warning string
	}{
		{sample, "v1.3.0", "## v1.3.0 (2026-10-20)\n\nShort summary.\n\n### Added\n- **A thing.** Details.\n", ""},
		{sample, "v1.2.0", "## v1.2.0 (2026-10-21)\n\n### Fixed\n- Undated.\n", "has no date"},
		{sample, "v9.0.0", "## v9.0.0 (2026-10-21)\n\n" + NoNotes + "\n", "no section for v9.0.0"},
		{sample, "v1.0.0", "## v1.0.0 (2026-09-01)\n\n" + NoNotes + "\n", ""},
		{"", "v1.0.0", "## v1.0.0 (2026-10-21)\n\n" + NoNotes + "\n", "no section"},
	}
	for _, c := range cases {
		body, warnings := Notes([]byte(c.text), c.version, published)
		if body != c.want {
			t.Errorf("%s: body\n%q\nwant\n%q", c.version, body, c.want)
		}
		if got := strings.Join(warnings, "; "); c.warning == "" && got != "" || !strings.Contains(got, c.warning) {
			t.Errorf("%s: warnings %q, want one with %q", c.version, got, c.warning)
		}
	}
	if _, warnings := Notes(nil, "v1.0.0", published); len(warnings) != 1 || !strings.Contains(warnings[0], "there's no CHANGELOG.md") {
		t.Errorf("no file: %q", warnings)
	}
	if body, warnings := NotesDated([]byte(sample), "v1.3.0", time.Date(2025, 1, 2, 0, 0, 0, 0, time.UTC)); !strings.HasPrefix(body, "## v1.3.0 (2025-01-02)\n\nShort summary.") || len(warnings) != 0 {
		t.Errorf("NotesDated = %q, %q", body, warnings)
	}
}
