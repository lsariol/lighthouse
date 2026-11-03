package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/lsariol/lighthouse/internal/projects"
)

// capture sends o's log lines to the returned buffer.
func capture(o *Orchestrator) *bytes.Buffer {
	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	o.log, o.reconcileLog, o.publishLog = log, log, log
	return &buf
}

// lines returns the logged records with this message.
func lines(t *testing.T, buf *bytes.Buffer, msg string) []map[string]any {
	t.Helper()
	var found []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("not JSON: %q: %v", line, err)
		}
		if record["msg"] == msg {
			found = append(found, record)
		}
	}
	return found
}

func TestCheckFailureLevels(t *testing.T) {
	t.Run("a passing failure is a warning", func(t *testing.T) {
		gh := newGitHub(map[string]string{"a": "aaa"})
		gh.repos["a"].down = true
		o, _, _, _ := setup(t, gh, "a")
		buf := capture(o)

		if err := o.Scan(context.Background()); !errors.Is(err, errChecksFailed) {
			t.Fatalf("Scan = %v", err)
		}
		got := lines(t, buf, "check failed")
		if len(got) != 1 || got[0]["level"] != "WARN" || got[0]["project"] != "a" || !strings.Contains(got[0]["err"].(string), "502") {
			t.Fatalf("logged %v", got)
		}
	})

	t.Run("a lasting failure is an error", func(t *testing.T) {
		o, _, _, _ := setup(t, newGitHub(nil), "gone")
		buf := capture(o)

		if err := o.Scan(context.Background()); !errors.Is(err, errChecksFailed) {
			t.Fatalf("Scan = %v", err)
		}
		if got := lines(t, buf, "check failed"); len(got) != 1 || got[0]["level"] != "ERROR" {
			t.Fatalf("logged %v", got)
		}
	})
}

// A failed deploy is logged by the deployer, so the check doesn't say it again.
func TestFailedDeployIsNotLoggedTwice(t *testing.T) {
	o, _, d, _ := setup(t, newGitHub(map[string]string{"a": "aaa"}), "a")
	d.result = failing(projects.FailurePermanent)
	buf := capture(o)

	if err := o.Scan(context.Background()); !errors.Is(err, errChecksFailed) {
		t.Fatalf("Scan = %v", err)
	}
	if got := lines(t, buf, "check failed"); len(got) != 0 {
		t.Fatalf("logged %v", got)
	}
}

func TestBrokenProjectIsAnError(t *testing.T) {
	o, _, d, _ := setup(t, newGitHub(map[string]string{"a": "aaa"}), "a")
	d.result = failing(projects.FailurePermanent)
	buf := capture(o)

	for range projects.BrokenAfter {
		o.Scan(context.Background())
	}
	const msg = "project is broken: its latest commit isn't tried again until a new one or `retry`"
	if got := lines(t, buf, msg); len(got) != 1 || got[0]["level"] != "ERROR" {
		t.Fatalf("logged %v in %s", got, buf)
	}
}
