package deploy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lsariol/lighthouse/internal/compose"
	"github.com/lsariol/lighthouse/internal/docker"
	"github.com/lsariol/lighthouse/internal/projects"
)

// newSelfEnv is Lighthouse deploying itself: compose project "lighthouse",
// one built service, no secrets.
func newSelfEnv(t *testing.T) *env {
	e := newEnv(t)
	e.d.self = "lighthouse"
	e.compose.name = "lighthouse"
	e.compose.services = []compose.Service{{Name: "lighthouse", Build: true}}
	e.compose.vars = []compose.Variable{{Name: DeployVersionVar, Default: "dev"}}
	e.compose.up = func(dir string) {
		e.docker.set("lighthouse", "lighthouse", "lh-"+filepath.Base(dir), "img-"+filepath.Base(dir), healthy)
	}
	return e
}

var lighthouse = projects.Project{Name: "lighthouse", Repo: landing.Repo}

func TestSelfDeployHandsOff(t *testing.T) {
	e := newSelfEnv(t)
	res := e.d.Deploy(context.Background(), Request{Project: lighthouse, SHA: sha1, Version: "v1.0.0", Trigger: projects.TriggerCheck})

	if !res.HandedOff || res.Err != nil || res.Status != "" {
		t.Fatalf("Deploy = %+v", res)
	}
	if len(e.compose.ups) != 0 {
		t.Error("Lighthouse swapped itself")
	}
	want := HelperName + " lighthouse-lighthouse:lh-" + sha1[:12] + " /lighthouse self-update " + filepath.Join(e.root, ".self-update", sha1[:12]+".json")
	if len(e.compose.helpers) != 1 || e.compose.helpers[0] != want {
		t.Errorf("helpers %v, want %q", e.compose.helpers, want)
	}
	h, err := readHandOff(filepath.Join(e.root, ".self-update", sha1[:12]+".json"))
	if err != nil || h.SHA != sha1 || h.Version != "v1.0.0" || h.Trigger != projects.TriggerCheck || h.Done || len(h.Steps) == 0 {
		t.Errorf("hand-off %+v, %v", h, err)
	}
	if last := res.Steps[len(res.Steps)-1]; last.Name != StepHandOff || last.Status != projects.StepSucceeded {
		t.Errorf("last step %+v", last)
	}

	if ready, _ := e.d.HandOffs(); len(ready) != 0 {
		t.Errorf("ready before the helper finished: %+v", ready)
	}
}

// newSelfEnvWithSecret is newSelfEnv with one secret, and a previous version
// running from a folder this Lighthouse keeps.
func newSelfEnvWithSecret(t *testing.T) (*env, projects.Project) {
	e := newSelfEnv(t)
	e.compose.vars = append(e.compose.vars, compose.Variable{Name: "LIGHTHOUSE_API_TOKEN"})
	e.secrets.values["LIGHTHOUSE_API_TOKEN"] = "tok-new-1234"
	p := lighthouse
	p.DeployedSHA = sha1
	os.MkdirAll(filepath.Join(e.root, "lighthouse", sha1[:12]), 0o755)
	e.docker.set("lighthouse", "lighthouse", "lh-old", "img-old", healthy)
	return e, p
}

// The hand-off names the secrets; their values reach the helper only in its
// environment, the previous version's too.
func TestSelfDeployCarriesSecrets(t *testing.T) {
	e, p := newSelfEnvWithSecret(t)
	res := e.d.Deploy(context.Background(), Request{Project: p, SHA: sha2})
	if !res.HandedOff || res.Err != nil {
		t.Fatalf("Deploy = %+v", res)
	}
	env := e.compose.helperEnv[0]
	if !slices.Contains(env, SecretPrefix+"LIGHTHOUSE_API_TOKEN=tok-new-1234") || !slices.Contains(env, PreviousSecretPrefix+"LIGHTHOUSE_API_TOKEN=tok-new-1234") || len(env) != 2 {
		t.Errorf("the helper's environment: %v", env)
	}
	path := filepath.Join(e.root, ".self-update", sha2[:12]+".json")
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "tok-new-1234") {
		t.Error("a secret value is in the hand-off file")
	}
	h, err := readHandOff(path)
	if err != nil || !slices.Equal(h.Secrets, []string{"LIGHTHOUSE_API_TOKEN"}) || !slices.Equal(h.PreviousSecrets, []string{"LIGHTHOUSE_API_TOKEN"}) {
		t.Errorf("hand-off secrets %v, previous %v, %v", h.Secrets, h.PreviousSecrets, err)
	}
}

// The helper swaps with the new values, and a rollback starts the previous
// version with its own.
func TestHelperUsesTheSecrets(t *testing.T) {
	e, p := newSelfEnvWithSecret(t)
	e.d.Deploy(context.Background(), Request{Project: p, SHA: sha2})
	t.Setenv(SecretPrefix+"LIGHTHOUSE_API_TOKEN", "tok-new-1234")
	t.Setenv(PreviousSecretPrefix+"LIGHTHOUSE_API_TOKEN", "tok-old-5678")
	e.compose.up = func(dir string) {
		if filepath.Base(dir) == sha2[:12] {
			e.docker.set("lighthouse", "lighthouse", "lh-new", "img-new", docker.Detail{State: "running", Health: "unhealthy", RestartPolicy: "unless-stopped"})
		} else {
			e.docker.set("lighthouse", "lighthouse", "lh-old", "img-old", healthy)
		}
	}
	e.d.FinishHandOff(context.Background(), filepath.Join(e.root, ".self-update", sha2[:12]+".json"))
	if len(e.compose.upEnv) != 2 || !slices.Contains(e.compose.upEnv[0], "LIGHTHOUSE_API_TOKEN=tok-new-1234") || !slices.Contains(e.compose.upEnv[1], "LIGHTHOUSE_API_TOKEN=tok-old-5678") {
		t.Errorf("swap and rollback environments: %v", e.compose.upEnv)
	}
	if ready, _ := e.d.HandOffs(); len(ready) != 1 || ready[0].Status != projects.StatusRolledBack {
		t.Fatalf("HandOffs = %+v", ready)
	}
	if len(e.secrets.asked) != 2 {
		t.Errorf("the helper asked Cove: %v", e.secrets.asked)
	}
}

// A helper started without its secrets changes nothing.
func TestHelperWithoutItsSecrets(t *testing.T) {
	e, p := newSelfEnvWithSecret(t)
	e.d.Deploy(context.Background(), Request{Project: p, SHA: sha2})
	err := e.d.FinishHandOff(context.Background(), filepath.Join(e.root, ".self-update", sha2[:12]+".json"))
	if err == nil || !strings.Contains(err.Error(), "LIGHTHOUSE_API_TOKEN") || len(e.compose.ups) != 0 {
		t.Errorf("FinishHandOff = %v, ups %v", err, e.compose.ups)
	}
	if ready, _ := e.d.HandOffs(); len(ready) != 1 || ready[0].Status != projects.StatusFailed || ready[0].FailedStep != StepHandOff {
		t.Errorf("HandOffs = %+v", ready)
	}
}

// Without the previous version's secrets a rollback couldn't start it, so
// the update waits and is tried again.
func TestSelfDeployWaitsForThePreviousSecrets(t *testing.T) {
	e, p := newSelfEnvWithSecret(t)
	e.secrets.err, e.secrets.failOn = errors.New("Cove: connection refused"), 2
	res := e.d.Deploy(context.Background(), Request{Project: p, SHA: sha2})
	if res.HandedOff || res.FailedStep != StepHandOff || res.FailureKind != projects.FailureTransient || len(e.compose.helpers) != 0 {
		t.Errorf("Deploy = %+v, helpers %v", res, e.compose.helpers)
	}
}

func TestHelperSwapsAndVerifies(t *testing.T) {
	e := newSelfEnv(t)
	p := lighthouse
	p.DeployedSHA = sha1
	os.MkdirAll(filepath.Join(e.root, "lighthouse", sha1[:12]), 0o755)
	e.docker.set("lighthouse", "lighthouse", "lh-old", "img-old", healthy)
	res := e.d.Deploy(context.Background(), Request{Project: p, SHA: sha2, Trigger: projects.TriggerManual})
	if !res.HandedOff {
		t.Fatalf("Deploy = %+v", res)
	}
	path := filepath.Join(e.root, ".self-update", sha2[:12]+".json")

	if err := e.d.FinishHandOff(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	if len(e.compose.ups) != 1 || filepath.Base(e.compose.ups[0]) != sha2[:12] {
		t.Errorf("ups %v", e.compose.ups)
	}
	if !slices.Contains(e.compose.upEnv[0], DeployCommitVar+"="+sha2) {
		t.Errorf("swap environment %v", e.compose.upEnv[0])
	}
	ready, err := e.d.HandOffs()
	if err != nil || len(ready) != 1 || ready[0].Status != projects.StatusSucceeded {
		t.Fatalf("HandOffs = %+v, %v", ready, err)
	}
	d := ready[0].Deployment()
	var names []string
	for _, s := range d.Steps {
		names = append(names, s.Name)
	}
	if d.SHA != sha2 || d.Trigger != projects.TriggerManual || !strings.Contains(strings.Join(names, " "), "build backup handoff swap verify cleanup") {
		t.Errorf("deployment %+v, steps %v", d, names)
	}
	removed := len(e.compose.removed)
	if err := e.d.Forget(ready[0]); err != nil {
		t.Fatal(err)
	}
	if ready, _ := e.d.HandOffs(); len(ready) != 0 {
		t.Error("forgotten, but still there")
	}
	if len(e.compose.removed) != removed+1 || e.compose.removed[removed] != HelperName {
		t.Errorf("after a successful update, removed %v; want the helper", e.compose.removed)
	}
	if err := e.d.FinishHandOff(context.Background(), path); err == nil {
		t.Error("finishing a forgotten hand-off worked")
	}
}

func TestHelperRollsBack(t *testing.T) {
	e := newSelfEnv(t)
	p := lighthouse
	p.DeployedSHA = sha1
	os.MkdirAll(filepath.Join(e.root, "lighthouse", sha1[:12]), 0o755)
	e.docker.set("lighthouse", "lighthouse", "lh-old", "img-old", healthy)
	e.d.Deploy(context.Background(), Request{Project: p, SHA: sha2})

	e.compose.up = func(dir string) {
		if filepath.Base(dir) == sha2[:12] {
			e.docker.set("lighthouse", "lighthouse", "lh-new", "img-new", docker.Detail{State: "running", Health: "unhealthy", RestartPolicy: "unless-stopped"})
		} else {
			e.docker.set("lighthouse", "lighthouse", "lh-old", "img-old", healthy)
		}
	}
	err := e.d.FinishHandOff(context.Background(), filepath.Join(e.root, ".self-update", sha2[:12]+".json"))
	if err == nil || !strings.Contains(err.Error(), "rolled back") {
		t.Errorf("FinishHandOff = %v", err)
	}
	ready, _ := e.d.HandOffs()
	if len(ready) != 1 || ready[0].Status != projects.StatusRolledBack || ready[0].FailedStep != StepVerify {
		t.Fatalf("HandOffs = %+v", ready)
	}
	if last := e.compose.ups[len(e.compose.ups)-1]; filepath.Base(last) != sha1[:12] || e.docker.tags["lighthouse-lighthouse"] != "img-old" {
		t.Errorf("rolled back in %s, image %q", last, e.docker.tags["lighthouse-lighthouse"])
	}
	removed := len(e.compose.removed)
	e.d.Forget(ready[0])
	if len(e.compose.removed) != removed {
		t.Error("a failed update's helper was removed: its logs say why it failed")
	}
}

func TestHelperRollsBackWithoutTheOldFolder(t *testing.T) {
	e := newSelfEnv(t)
	p := lighthouse
	p.DeployedSHA = sha1
	e.docker.set("lighthouse", "lighthouse", "lh-old", "img-old", healthy)
	e.d.Deploy(context.Background(), Request{Project: p, SHA: sha2})
	e.compose.up = func(dir string) {
		e.docker.set("lighthouse", "lighthouse", "lh-x", "img-x", docker.Detail{State: "exited", ExitCode: 1, RestartPolicy: "unless-stopped"})
	}
	e.d.FinishHandOff(context.Background(), filepath.Join(e.root, ".self-update", sha2[:12]+".json"))
	ready, _ := e.d.HandOffs()
	if len(ready) != 1 || ready[0].Status != projects.StatusRolledBack || len(e.compose.ups) != 2 || e.docker.tags["lighthouse-lighthouse"] != "img-old" {
		t.Errorf("HandOffs %+v, ups %v, tags %v", ready, e.compose.ups, e.docker.tags)
	}
}

func TestStaleHandOff(t *testing.T) {
	e := newSelfEnv(t)
	e.d.Deploy(context.Background(), Request{Project: lighthouse, SHA: sha1})
	path := filepath.Join(e.root, ".self-update", sha1[:12]+".json")
	h, _ := readHandOff(path)
	h.StartedAt = time.Now().Add(-HandOffWait - time.Minute)
	writeHandOff(h)

	ready, _ := e.d.HandOffs()
	if len(ready) != 1 || ready[0].Status != projects.StatusFailed || !strings.Contains(ready[0].Error, "didn't finish") {
		t.Errorf("a stale hand-off: %+v", ready)
	}
}

func TestHelperFailsToStart(t *testing.T) {
	e := newSelfEnv(t)
	e.compose.runErr = errors.New("docker run failed (exit status 125): Conflict. The container name is already in use")
	res := e.d.Deploy(context.Background(), Request{Project: lighthouse, SHA: sha1})
	if res.HandedOff || res.FailedStep != StepHandOff || res.FailureKind != projects.FailureTransient || !strings.Contains(res.Err.Error(), "start the helper") {
		t.Errorf("Deploy = %+v", res)
	}
	if ready, _ := e.d.HandOffs(); len(ready) != 0 {
		t.Error("a hand-off was left behind")
	}
}

func TestOneHandOffAtATime(t *testing.T) {
	e := newSelfEnv(t)
	if res := e.d.Deploy(context.Background(), Request{Project: lighthouse, SHA: sha1}); !res.HandedOff {
		t.Fatalf("first Deploy = %+v", res)
	}
	if !e.d.HandOffInProgress("LIGHTHOUSE") || e.d.HandOffInProgress("plop") {
		t.Error("HandOffInProgress doesn't see the hand-off (or sees it for another project)")
	}
	res := e.d.Deploy(context.Background(), Request{Project: lighthouse, SHA: sha1})
	if res.HandedOff || res.FailedStep != StepHandOff || res.FailureKind != projects.FailureTransient || !strings.Contains(res.Err.Error(), "already in progress") {
		t.Errorf("second Deploy = %+v", res)
	}
	if len(e.compose.helpers) != 1 || len(res.Steps) != 1 {
		t.Errorf("helpers started: %v; steps %v (want only the refusal, before anything is downloaded)", e.compose.helpers, stepStatuses(res))
	}
	if _, err := os.Stat(filepath.Join(e.root, "lighthouse", sha1[:12], "compose.yaml")); err != nil {
		t.Errorf("the refused deploy touched the helper's files: %v", err)
	}

	e.d.FinishHandOff(context.Background(), filepath.Join(e.root, ".self-update", sha1[:12]+".json"))
	if e.d.HandOffInProgress("lighthouse") {
		t.Error("still in progress after the helper finished")
	}
}
