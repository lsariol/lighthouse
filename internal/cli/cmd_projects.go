package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/lsariol/lighthouse/internal/control"
)

func (c *CLI) list(ctx context.Context, args []string) error {
	all, rest := takeFlag(args[1:], "--all", "-a")
	if len(rest) != 0 {
		return usageError{form: "list [--all]"}
	}

	projects, err := c.svc.Projects(ctx)
	if err != nil {
		return err
	}
	if len(projects) == 0 {
		info("No projects are watched yet. Add one with \"add <url>\" (see \"help setup\").")
		return nil
	}
	slices.SortStableFunc(projects, newestDeployFirst)

	rows := [][]string{{"NAME", "MODE", "VERSION", "DEPLOYED"}}
	if all {
		rows[0] = []string{"NAME", "REPOSITORY", "MODE", "VERSION", "DEPLOYED", "CHECKED"}
	}
	for _, p := range projects {
		row := []string{p.Name, mode(p), version(p), formatTime(p.LastDeployed)}
		if all {
			row = []string{p.Name, p.URL, mode(p), version(p), formatTime(p.LastDeployed), formatTime(p.LastChecked)}
		}
		rows = append(rows, row)
	}
	table(rows)
	info(fmt.Sprintf("%d %s", len(projects), plural(len(projects), "project", "projects")))

	for _, p := range projects {
		if p.Broken {
			warn(fmt.Sprintf("%q is broken: its latest commit failed %d times. \"report %s\" shows why; \"retry %s\" tries again.", p.Name, p.FailureCount, p.Name, p.Name))
		}
		if p.LastError != "" {
			warn(fmt.Sprintf("%q, last check %s: %s", p.Name, formatTime(p.LastErrorAt), p.LastError))
		}
		for _, line := range releaseWarnings(p) {
			warn(line)
		}
	}
	return nil
}

// mode says what Lighthouse does with p, and its tier if it isn't an app:
// "tags", "commits, infra", "publish-only".
func mode(p control.Project) string {
	if p.Kind == "publish" {
		return "publish-only"
	}
	s := "commits"
	if p.Mode == "releases" {
		s = "tags"
	}
	if p.Tier != "" && p.Tier != "app" {
		s += ", " + p.Tier
	}
	return s
}

// version is what p runs or last published: its release, else its commit,
// or "-"; ", stopped" when stopped on purpose.
func version(p control.Project) string {
	v := p.Version
	if v == "" {
		v = shortSHA(p.Commit)
	}
	if p.Stopped {
		v += ", stopped"
	}
	return v
}

// newestDeployFirst orders projects by last deploy, newest first, with
// never-deployed projects last.
func newestDeployFirst(a, b control.Project) int {
	switch {
	case a.LastDeployed == nil && b.LastDeployed == nil:
		return 0
	case a.LastDeployed == nil:
		return 1
	case b.LastDeployed == nil:
		return -1
	}
	return b.LastDeployed.Compare(*a.LastDeployed)
}

func (c *CLI) add(ctx context.Context, args []string) error {
	const form = "add <url> [--name <name>] [--publish-only]"
	publishOnly, rest := takeFlag(args[1:], "--publish-only")
	name, rest, err := takeValue(rest, "--name")
	if err != nil || len(rest) != 1 {
		return usageError{form: form}
	}
	url := rest[0]

	project, err := c.svc.Add(ctx, name, url, publishOnly)

	var ce *control.Error
	for name == "" && errors.As(err, &ce) && ce.Kind == control.KindNameTaken {
		if c.term == nil && !c.interactive {
			return err
		}
		warn(err.Error())
		answer, answered := c.askLine("Name for this project (Enter cancels):")
		if !answered || answer == "" {
			info("Add cancelled.")
			return nil
		}
		project, err = c.svc.Add(ctx, answer, url, publishOnly)
	}
	if err != nil {
		return err
	}

	if publishOnly {
		success(fmt.Sprintf("Publishing %q's GitHub Releases (%s); it's never deployed. Its tags so far are left as they are; each new version tag on main gets a Release.", project.Name, project.URL))
		info(fmt.Sprintf("The GitHub App must be installed on the repository. \"releases %s\" shows its tags after the next check.", project.Name))
		return nil
	}
	success(fmt.Sprintf("Watching %q (%s). It deploys on the next check; \"scan\" checks now.", project.Name, project.URL))
	info("What the repository needs: \"help setup\".")
	return nil
}

func (c *CLI) remove(ctx context.Context, args []string) error {
	skip, rest := takeYesFlag(args[1:])
	keep, rest := takeFlag(rest, "--keep")
	if len(rest) != 1 {
		return usageError{form: "remove <name> [--keep] [--yes]"}
	}
	name := rest[0]

	question := fmt.Sprintf("Stop watching %q, and stop and remove its containers?", name)
	if keep {
		question = fmt.Sprintf("Stop watching %q? Its containers keep running.", name)
	}
	ok, err := c.confirmOrRefuse(question, skip, "remove")
	if !ok || err != nil {
		return err
	}

	if err := c.svc.Remove(ctx, name, !keep); err != nil {
		return err
	}
	if keep {
		success(fmt.Sprintf("Stopped watching %q. Its containers keep running, untracked.", name))
	} else {
		success(fmt.Sprintf("Removed %q and its containers.", name))
	}
	return nil
}

func (c *CLI) rename(ctx context.Context, args []string) error {
	if len(args) != 3 {
		return usageError{form: "rename <name> <new-name>"}
	}
	if err := c.svc.Rename(ctx, args[1], args[2]); err != nil {
		return err
	}
	success(fmt.Sprintf("Renamed %q to %q.", args[1], args[2]))
	return nil
}

func (c *CLI) setURL(ctx context.Context, args []string) error {
	if len(args) != 3 {
		return usageError{form: "set-url <name> <url>"}
	}
	if err := c.svc.SetURL(ctx, args[1], args[2]); err != nil {
		return err
	}
	success(fmt.Sprintf("%q now watches %s. It deploys on the next check.", args[1], args[2]))
	return nil
}
