// Package policy decides whether a compose file may be deployed (DOCUMENTATION.md §7.1).
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

// SharedNetwork is the network projects share, to reach Cove, sparkdb and
// each other. Every other network a project joins must be its own.
const SharedNetwork = "spark"

// SharedPrefix starts the Cove keys any project may use.
const SharedPrefix = "SHARED_"

// Policy is policy.json: the exceptions to the rules.
type Policy struct {
	Exceptions []Exception `json:"exceptions"`
}

// Exception allows a compose project, or one of its services, what the rules
// would refuse.
type Exception struct {
	Project string   `json:"project"`
	Service string   `json:"service,omitempty"` // only this service's findings; "" for the whole project
	Allow   []string `json:"allow"`
	Reason  string   `json:"reason"`
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
		case e.Service != "" && !serviceName.MatchString(e.Service):
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
		}
	}
	return p, nil
}

// allowed returns the reason project's service may have the finding id, if
// it may.
func (p Policy) allowed(project string, service string, id string) (string, bool) {
	for _, e := range p.Exceptions {
		if e.Project != project || e.Service != "" && e.Service != service {
			continue
		}
		for _, a := range e.Allow {
			if a == id || (strings.HasSuffix(a, "*") && strings.HasPrefix(id, strings.TrimSuffix(a, "*"))) {
				return e.Reason, true
			}
		}
	}
	return "", false
}

// Apply marks the findings project has an exception for, and returns the
// ones that stop the deploy: everything that's neither a warning nor allowed.
func (p Policy) Apply(project string, findings []Finding) []Finding {
	var refused []Finding
	for i := range findings {
		f := &findings[i]
		if f.Warning {
			continue
		}
		if reason, ok := p.allowed(project, f.Service, f.ID); ok {
			f.Allowed = reason
			continue
		}
		refused = append(refused, *f)
	}
	return refused
}

// UnknownServices warns about exceptions for project that name a service its
// compose file doesn't have, so a misspelled one doesn't quietly do nothing.
func (p Policy) UnknownServices(project string, services []string) []Finding {
	var warnings []Finding
	for _, e := range p.Exceptions {
		if e.Project == project && e.Service != "" && !slices.Contains(services, e.Service) {
			warnings = append(warnings, Finding{Warning: true,
				Problem: fmt.Sprintf("policy.json has an exception for service %q, which the compose file doesn't have (its services: %s)", e.Service, strings.Join(services, ", "))})
		}
	}
	return warnings
}

// Finding is one thing a compose file asks for that the rules refuse (or,
// for a warning, advise against).
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
