package cli

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/lsariol/lighthouse/internal/control"
)

// waitingGrace is how long a tag may wait for main before status mentions it.
const waitingGrace = 10 * time.Minute

// unpublishedAfter is how many passing failures in a row to publish a
// Release status mentions.
const unpublishedAfter = 3

// publish publishes a GitHub Release for an existing tag, without deploying it.
func (c *CLI) publish(ctx context.Context, args []string) error {
	const form = "publish <name> <tag> [--date YYYY-MM-DD] [--notes-from <ref>] [--yes]"
	skip, rest := takeYesFlag(args[1:])
	date, rest, err := takeValue(rest, "--date")
	if err != nil {
		return usageError{form: form}
	}
	notesFrom, rest, err := takeValue(rest, "--notes-from")
	if err != nil || len(rest) != 2 {
		return usageError{form: form}
	}
	name, tag := rest[0], rest[1]

	question := fmt.Sprintf("Publish a GitHub Release for %q %s? It isn't verified by a deploy.", name, tag)
	ok, err := c.confirmOrRefuse(question, skip, "publish")
	if !ok || err != nil {
		return err
	}

	r, err := c.svc.Publish(ctx, name, tag, date, notesFrom)
	if err != nil {
		return err
	}
	switch r.Status {
	case "published":
		success(fmt.Sprintf("Published %q %s: %s", name, tag, r.URL))
		if r.Note != "" {
			info(sentence(r.Note))
		}
	default:
		warn(fmt.Sprintf("%q %s isn't published yet: %s (%s). \"releases %s\" shows how it goes.", name, tag,
			strings.TrimSuffix(firstLine(orDash(r.Error+r.Note), 200), "."), nextTry(r), name))
	}
	return nil
}

// releases lists a project's version tags, or shows what one tag's Release will say.
func (c *CLI) releases(ctx context.Context, args []string) error {
	switch len(args) {
	case 2:
	case 3:
		return c.releaseNotes(ctx, args[1], args[2])
	default:
		return usageError{form: "releases <name> [tag]"}
	}

	list, err := c.svc.Releases(ctx, args[1])
	if err != nil {
		return err
	}
	if len(list) == 0 {
		info(fmt.Sprintf("%q has no version tags tracked. Lighthouse tracks them for projects that deploy releases (x-lighthouse: deploy: releases).", args[1]))
		return nil
	}
	rows := [][]string{{"TAG", "COMMIT", "STATUS", "SINCE", "NOTE"}}
	for _, r := range list {
		since := r.Since
		rows = append(rows, []string{r.Tag, shortSHA(r.Commit), r.Status, formatTime(&since), orDash(releaseNote(r))})
	}
	table(rows)
	info(fmt.Sprintf("%d %s. \"releases %s <tag>\" shows what a tag's GitHub Release will say.", len(list), plural(len(list), "tag", "tags"), args[1]))
	return nil
}

func (c *CLI) releaseNotes(ctx context.Context, name string, tag string) error {
	n, err := c.svc.ReleaseNotes(ctx, name, tag)
	if err != nil {
		return err
	}
	out(strings.TrimSuffix(n.Body, "\n"))
	for _, w := range n.Warnings {
		warn(sentence(w))
	}
	return nil
}

// releaseNote is what the releases table says about r: its Release, the
// last attempt's error and the next one, or its note.
func releaseNote(r control.Release) string {
	var parts []string
	if r.URL != "" {
		parts = append(parts, r.URL)
	}
	if r.Error != "" {
		parts = append(parts, fmt.Sprintf("%s (%s)", firstLine(r.Error, 80), nextTry(r)))
	}
	if r.Note != "" {
		parts = append(parts, r.Note)
	}
	return strings.Join(parts, "; ")
}

// nextTry says when publishing r is tried again.
func nextTry(r control.Release) string {
	if r.NextAttempt == nil {
		return "not tried again"
	}
	return "tried again " + formatTime(r.NextAttempt)
}

// releaseWarnings are list's lines about a project's releases.
func releaseWarnings(p control.Project) []string {
	var lines []string
	for _, r := range p.OpenReleases {
		switch {
		case r.Error != "" && r.Status == "failed":
			lines = append(lines, fmt.Sprintf("%q %s failed to publish: %s (%s).", p.Name, r.Tag, strings.TrimSuffix(firstLine(r.Error, 200), "."), nextTry(r)))
		case r.Error != "":
			lines = append(lines, fmt.Sprintf("%q %s isn't published yet: %s (%s).", p.Name, r.Tag, strings.TrimSuffix(firstLine(r.Error, 200), "."), nextTry(r)))
		default:
			lines = append(lines, fmt.Sprintf("%q %s: %s.", p.Name, r.Tag, r.Note))
		}
	}
	if p.NoRelease {
		lines = append(lines, fmt.Sprintf("%q deploys releases but has no version tag yet: tag a commit on main (v1.0.0) and push the tag.", p.Name))
	}
	return lines
}

// releaseProblems are status's "Needs attention" items about a project's
// releases: tags waiting for main longer than waitingGrace, moved tags, and
// no release yet.
func releaseProblems(p control.Project, now time.Time) []string {
	var problems []string
	for _, r := range p.OpenReleases {
		since := r.Since
		switch {
		case r.Status == "moved":
			problems = append(problems, fmt.Sprintf("%q %s's tag moved, so it isn't deployed or published", p.Name, r.Tag))
		case r.Status == "failed":
			problems = append(problems, fmt.Sprintf("%q %s failed to publish (\"releases %s\"): %s", p.Name, r.Tag, p.Name, strings.TrimSuffix(firstLine(r.Error, 200), ".")))
		case r.Status == "pending" && r.Attempts >= unpublishedAfter:
			problems = append(problems, fmt.Sprintf("%q %s failed to publish %d times in a row: %s", p.Name, r.Tag, r.Attempts, strings.TrimSuffix(firstLine(r.Error, 200), ".")))
		case r.Status == "waiting" && now.Sub(r.Since) >= waitingGrace:
			problems = append(problems, fmt.Sprintf("%q %s has waited since %s for main to contain it", p.Name, r.Tag, formatTime(&since)))
		}
	}
	if p.NoRelease {
		problems = append(problems, fmt.Sprintf("%q has no release yet (tag a commit on main, e.g. v1.0.0)", p.Name))
	}
	return problems
}

// sentence capitalizes s and ends it with a period.
func sentence(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	s = string(unicode.ToUpper(r)) + s[n:]
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}
