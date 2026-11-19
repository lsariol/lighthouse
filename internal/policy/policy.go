// Package policy decides whether a compose file may be deployed: the rules and policy.json's exceptions.
package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// SharedNetwork is the network projects share; every other network a project joins must be its own.
const SharedNetwork = "spark"

// SharedPrefix starts the Cove keys any project may use.
const SharedPrefix = "SHARED_"

// Policy is policy.json: the exceptions to the rules.
type Policy struct {
	Exceptions []Exception `json:"exceptions"`
}

// Exception allows one service what the rules refuse; mount:<path>:ro is read-only only, :rw either way.
type Exception struct {
	Project string   `json:"project"`
	Service string   `json:"service"`
	Allow   []string `json:"allow"`
	Reason  string   `json:"reason"`
}

// The modes a host mount's finding ends with.
const (
	ReadOnly  = "ro"
	ReadWrite = "rw"
)

// MountID is a host mount's finding: mount:<path>:ro or mount:<path>:rw.
func MountID(path string, readOnly bool) string {
	if readOnly {
		return "mount:" + path + ":" + ReadOnly
	}
	return "mount:" + path + ":" + ReadWrite
}

// splitMode splits a host mount's ID into the rest and its mode, "" if it has none.
func splitMode(id string) (string, string) {
	if !strings.HasPrefix(id, "mount:") {
		return id, ""
	}
	for _, mode := range []string{ReadOnly, ReadWrite} {
		if rest, ok := strings.CutSuffix(id, ":"+mode); ok {
			return rest, mode
		}
	}
	return id, ""
}

var (
	composeName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	serviceName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)
)

// Parse reads policy.json.
func Parse(data []byte) (Policy, error) {
	var p Policy
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Policy{}, fmt.Errorf("policy.json: %v", err)
	}
	for i, e := range p.Exceptions {
		switch {
		case !composeName.MatchString(e.Project):
			return Policy{}, fmt.Errorf("policy.json: exception %d: project %q isn't a compose project name (lowercase letters, digits, - and _)", i+1, e.Project)
		case e.Service == "":
			return Policy{}, fmt.Errorf("policy.json: exception %d (%s): give the service it's for; an exception covers one service, so a service added later gets nothing by accident", i+1, e.Project)
		case !serviceName.MatchString(e.Service):
			return Policy{}, fmt.Errorf("policy.json: exception %d (%s): service %q isn't a compose service name (letters, digits, -, _ and .)", i+1, e.Project, e.Service)
		case len(e.Allow) == 0:
			return Policy{}, fmt.Errorf("policy.json: exception %d (%s): allow is empty", i+1, e.Project)
		case strings.TrimSpace(e.Reason) == "":
			return Policy{}, fmt.Errorf("policy.json: exception %d (%s): give a reason", i+1, e.Project)
		}
		for _, id := range e.Allow {
			if id == "" || id == "*" {
				return Policy{}, fmt.Errorf("policy.json: exception %d (%s): %q allows too much; name the finding IDs", i+1, e.Project, id)
			}
			rest, mode := splitMode(id)
			switch {
			case strings.HasPrefix(id, "mount:") && mode == "":
				return Policy{}, fmt.Errorf("policy.json: exception %d (%s): %q needs :ro or :rw at the end, to say whether the mount may write", i+1, e.Project, id)
			case mode == ReadOnly && strings.HasSuffix(rest, ".sock"):
				return Policy{}, fmt.Errorf("policy.json: exception %d (%s): %q: a socket mounted read-only still takes every request sent to it, so :ro protects nothing; allow it as :rw", i+1, e.Project, id)
			}
		}
	}
	return p, nil
}

// allowed returns why service may have finding id, and whether a read-write mount is allowed read-only only.
func (p Policy) allowed(project string, service string, id string) (reason string, ok bool, readOnlyOnly bool) {
	rest, mode := splitMode(id)
	for _, e := range p.Exceptions {
		if e.Project != project || e.Service != service {
			continue
		}
		for _, a := range e.Allow {
			aRest, aMode := splitMode(a)
			if aRest != rest && !(strings.HasSuffix(aRest, "*") && strings.HasPrefix(rest, strings.TrimSuffix(aRest, "*"))) {
				continue
			}
			if aMode == ReadOnly && mode != ReadOnly {
				readOnlyOnly = true
				continue
			}
			return e.Reason, true, false
		}
	}
	return "", false, readOnlyOnly
}

// Apply marks the allowed findings and returns those that stop the deploy.
func (p Policy) Apply(project string, findings []Finding) []Finding {
	var refused []Finding
	for i := range findings {
		f := &findings[i]
		if f.Warning {
			continue
		}
		reason, ok, readOnlyOnly := p.allowed(project, f.Service, f.ID)
		if ok {
			f.Allowed = reason
			continue
		}
		if readOnlyOnly {
			f.Problem += "; policy.json allows it read-only only, so mount it read-only (:ro)"
		}
		refused = append(refused, *f)
	}
	return refused
}

// UnknownServices warns about exceptions naming a service the compose file doesn't have.
func (p Policy) UnknownServices(project string, services []string) []Finding {
	var warnings []Finding
	for _, e := range p.Exceptions {
		if e.Project == project && !slices.Contains(services, e.Service) {
			warnings = append(warnings, Finding{Warning: true,
				Problem: fmt.Sprintf("policy.json has an exception for service %q, which the compose file doesn't have (its services: %s)", e.Service, strings.Join(services, ", "))})
		}
	}
	return warnings
}

// Finding is one thing a compose file asks for that the rules refuse or advise against.
type Finding struct {
	ID      string
	Service string
	Problem string
	Warning bool
	Allowed string
}

func (f Finding) String() string {
	where := ""
	if f.Service != "" {
		where = f.Service + ": "
	}
	switch {
	case f.Warning:
		return "! " + where + f.Problem
	case f.Allowed != "":
		return fmt.Sprintf("✓ %s%s [%s], allowed by policy.json: %s", where, f.Problem, f.ID, f.Allowed)
	default:
		return fmt.Sprintf("✗ %s%s [%s]", where, f.Problem, f.ID)
	}
}

// Summary describes refused findings in one line, for the deploy's error.
func Summary(refused []Finding) string {
	ids := make([]string, len(refused))
	for i, f := range refused {
		ids[i] = f.ID
	}
	sort.Strings(ids)
	return fmt.Sprintf("the compose file breaks the deploy rules (%s). Change the compose file, or add an exception to policy.json in Lighthouse's repository", strings.Join(ids, ", "))
}

// ErrConfig means the compose file's config couldn't be read.
var ErrConfig = errors.New("unexpected compose config")
