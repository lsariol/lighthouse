// Package changelog reads a version's section of a CHANGELOG.md and turns it
// into GitHub Release notes.
package changelog

import (
	"regexp"
	"strings"
	"time"
)

// File is the CHANGELOG's name at the top of a repository.
const File = "CHANGELOG.md"

// NoNotes is the body of a version without a section.
const NoNotes = "No notes for this version."

// Section is one version's part of a CHANGELOG.
type Section struct {
	Date string // YYYY-MM-DD, or "" if the heading has none
	Body string // what follows the heading, trimmed
}

var (
	heading = regexp.MustCompile(`^##\s+\[?v?(\S+?)\]?(?:\s|$)`)
	date    = regexp.MustCompile(`\b(\d{4}-\d{2}-\d{2})\b`)
)

// Find returns the section for version (v1.2.3 or 1.2.3), with headings
// written as "## v1.2.3 (2026-10-20)", "## v1.2.3" or "## [1.2.3] — 2026-10-20".
func Find(text []byte, version string) (Section, bool) {
	want := strings.TrimPrefix(version, "v")
	lines := strings.Split(strings.ReplaceAll(string(text), "\r\n", "\n"), "\n")
	for i, line := range lines {
		m := heading.FindStringSubmatch(line)
		if m == nil || m[1] != want {
			continue
		}
		s := Section{}
		if d := date.FindStringSubmatch(line[len(m[0]):]); d != nil {
			if _, err := time.Parse(time.DateOnly, d[1]); err == nil {
				s.Date = d[1]
			}
		}
		end := len(lines)
		for j := i + 1; j < len(lines); j++ {
			if strings.HasPrefix(lines[j], "## ") || lines[j] == "##" {
				end = j
				break
			}
		}
		s.Body = strings.TrimSpace(strings.Join(lines[i+1:end], "\n"))
		return s, true
	}
	return Section{}, false
}

// Notes is a GitHub Release's text for version, with the dated heading the
// website reads: the CHANGELOG's date, else published. Warnings say what was
// missing.
func Notes(text []byte, version string, published time.Time) (string, []string) {
	return notes(text, version, published, false)
}

// NotesDated is Notes with date in the heading, whatever the CHANGELOG says
// (a backfill keeping a version's original date).
func NotesDated(text []byte, version string, date time.Time) (string, []string) {
	return notes(text, version, date, true)
}

func notes(text []byte, version string, published time.Time, force bool) (string, []string) {
	var warnings []string
	s, found := Find(text, version)
	switch {
	case text == nil:
		warnings = append(warnings, "there's no "+File+"; the notes say \""+NoNotes+"\"")
	case !found:
		warnings = append(warnings, File+" has no section for "+version+"; the notes say \""+NoNotes+"\"")
	}
	if force {
		s.Date = published.Format(time.DateOnly)
	}
	if s.Date == "" {
		s.Date = published.Format(time.DateOnly)
		if found {
			warnings = append(warnings, "the "+version+" heading in "+File+" has no date (YYYY-MM-DD); the publication date is used")
		}
	}
	if s.Body == "" {
		s.Body = NoNotes
	}
	return "## " + version + " (" + s.Date + ")\n\n" + s.Body + "\n", warnings
}
