package deploy

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/lsariol/lighthouse/internal/compose"
	"github.com/lsariol/lighthouse/internal/docker"
	"github.com/lsariol/lighthouse/internal/projects"
)

// TestRealSelfUpdate runs a whole self-update against real Docker: a project
// built FROM the Lighthouse image hands off to the real update helper, which
// swaps it; then a version that never becomes healthy is handed off and must
// be rolled back. Each version reads a secret under its own key, so the
// rollback must give the previous version its own.
func TestRealSelfUpdate(t *testing.T) {
	base := os.Getenv("LIGHTHOUSE_E2E_SELF_UPDATE")
	if base == "" {
		t.Skip("set by scripts/e2e-self-update.sh, which runs it inside a container")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	const project = "lhselftest"
	root := "/tmp/lhtest/staging"
	os.RemoveAll(root)
	files := func(healthy bool, key string) map[string]string {
		check := `["CMD", "/lighthouse", "health"]`
		if !healthy {
			check = `["CMD", "false"]`
		}
		return map[string]string{
			"compose.yaml": `name: ` + project + `
services:
  app:
    build: .
    restart: unless-stopped
    environment:
      - APP_ENV=e2e
      - COVE_URL=http://127.0.0.1:1
      - COVE_TOKEN_PATH=/tmp/token
      - STAGING_PATH=/tmp/staging
      - LIGHTHOUSE_VERSION=${LIGHTHOUSE_DEPLOY_VERSION:-dev}
      - E2E_TOKEN=${` + key + `}
    healthcheck:
      test: ` + check + `
      interval: 1s
      retries: 3
      start_period: 3s
`,
			"Dockerfile": "FROM " + base + "\nCMD [\"/lighthouse\", \"serve\"]\n",
		}
	}

	runner := compose.Runner{}
	d, err := docker.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		var out bytes.Buffer
		runner.Down(context.Background(), t.TempDir(), project, &out)
		runner.RemoveContainer(context.Background(), HelperName)
		for _, sha := range []string{sha1, sha2} {
			d.Untag(context.Background(), project+"-app:lh-"+sha[:12])
		}
		d.Untag(context.Background(), project+"-app")
	})

	source := &fakeSource{}
	var log bytes.Buffer
	secrets := &fakeSecrets{values: map[string]string{"LHSELFTEST_TOKEN": "e2e-secret-one", "LHSELFTEST_TOKEN_TWO": "e2e-secret-two"}}
	dep := New(runner, d, source, secrets, Options{
		Root: root, Self: project, Output: &log, Poll: 500 * time.Millisecond,
		Timeouts: Timeouts{Verify: 60 * time.Second, Stable: 2 * time.Second},
	})
	p := projects.Project{Name: project, Repo: landing.Repo}

	printenv := func(name string) string {
		out, _ := exec.Command("docker", "exec", project+"-app-1", "printenv", name).Output()
		return strings.TrimSpace(string(out))
	}
	running := func() string { return printenv("LIGHTHOUSE_VERSION") }
	helperExists := func() bool { return exec.Command("docker", "inspect", HelperName).Run() == nil }
	finish := func(sha string, version string, healthy bool, key string) HandOff {
		t.Helper()
		source.archive = tarball(t, files(healthy, key))
		res := dep.Deploy(ctx, Request{Project: p, SHA: sha, Version: version, Trigger: projects.TriggerCheck})
		if !res.HandedOff {
			t.Fatalf("Deploy %s = %+v\n%s", version, res, log.String())
		}
		if again := dep.Deploy(ctx, Request{Project: p, SHA: sha, Version: version}); again.HandedOff || again.FailedStep != StepHandOff {
			t.Errorf("a second hand-off during the first: %+v", again)
		}
		for {
			out, err := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.State.Running}}", HelperName).Output()
			if err != nil || strings.TrimSpace(string(out)) == "false" {
				break
			}
			time.Sleep(time.Second)
		}
		ready, err := dep.HandOffs()
		if err != nil || len(ready) != 1 {
			logs, _ := exec.Command("docker", "logs", HelperName).CombinedOutput()
			t.Fatalf("HandOffs = %+v, %v\nhelper:\n%s", ready, err, logs)
		}
		if data, _ := os.ReadFile(ready[0].Path); strings.Contains(string(data), "e2e-secret") {
			t.Error("a secret value is in the hand-off file")
		}
		if label, _ := exec.Command("docker", "inspect", "-f", `{{index .Config.Labels "com.docker.compose.project"}}`, HelperName).Output(); strings.TrimSpace(string(label)) != "" {
			t.Errorf("the helper is labelled as compose project %q", strings.TrimSpace(string(label)))
		}
		if logs, _ := exec.Command("docker", "logs", HelperName).CombinedOutput(); strings.Contains(string(logs), "e2e-secret") {
			t.Errorf("a secret value is in the helper's output:\n%s", logs)
		}
		dep.Forget(ready[0])
		return ready[0]
	}
	h := finish(sha1, "v1.0.0", true, "LHSELFTEST_TOKEN")
	if h.Status != projects.StatusSucceeded || running() != "v1.0.0" || printenv("E2E_TOKEN") != "e2e-secret-one" {
		logs, _ := exec.Command("docker", "logs", HelperName).CombinedOutput()
		t.Fatalf("v1: %s (%s), running %q\nhelper:\n%s", h.Status, h.Error, running(), logs)
	}
	if helperExists() {
		t.Error("the helper of a successful update is still there, with the secrets it was given")
	}
	p.DeployedSHA, p.DeployedVersion = sha1, "v1.0.0"

	h = finish(sha2, "v2.0.0", false, "LHSELFTEST_TOKEN_TWO")
	if h.Status != projects.StatusRolledBack || h.FailedStep != StepVerify {
		logs, _ := exec.Command("docker", "logs", HelperName).CombinedOutput()
		t.Fatalf("v2: %s at %s (%s)\nhelper:\n%s", h.Status, h.FailedStep, h.Error, logs)
	}
	if running() != "v1.0.0" || printenv("E2E_TOKEN") != "e2e-secret-one" {
		t.Errorf("after the rollback, running %q with E2E_TOKEN %q", running(), printenv("E2E_TOKEN"))
	}
	if !helperExists() {
		t.Error("the helper of a failed update is gone, and its logs with it")
	}
}
