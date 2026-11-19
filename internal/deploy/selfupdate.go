package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lsariol/lighthouse/internal/compose"
	"github.com/lsariol/lighthouse/internal/projects"
)

// Self-update: Lighthouse deploying itself, swapped by a helper container started from the new image.

// HelperName is the update helper's container.
const HelperName = "lighthouse-updater"

// StepHandOff hands a self-update to the helper.
const StepHandOff = "handoff"

// HandOffWait is how long a hand-off may stay unfinished before it's recorded as failed.
const HandOffWait = 30 * time.Minute

// HandOff is a self-update in progress: what the helper needs, and how it went.
type HandOff struct {
	Path string `json:"-"`

	Project        projects.Project  `json:"project"`
	SHA            string            `json:"sha"`
	Version        string            `json:"version,omitempty"`
	Trigger        string            `json:"trigger"`
	ComposeProject string            `json:"composeProject"`
	Dir            string            `json:"dir"`
	Services       []compose.Service `json:"services"`
	Previous       map[string]string `json:"previous"`
	StartedAt      time.Time         `json:"startedAt"`
	Steps          []projects.Step   `json:"steps"`

	// Secrets are names only: the values reach the helper in its environment, never on disk.
	Secrets         []string `json:"secrets,omitempty"`
	PreviousSecrets []string `json:"previousSecrets,omitempty"`

	Done        bool      `json:"done,omitempty"`
	Status      string    `json:"status,omitempty"`
	FailureKind string    `json:"failureKind,omitempty"`
	FailedStep  string    `json:"failedStep,omitempty"`
	Error       string    `json:"error,omitempty"`
	FinishedAt  time.Time `json:"finishedAt,omitempty"`
}

// Deployment is the hand-off as a deployment to record.
func (h HandOff) Deployment() projects.Deployment {
	return projects.Deployment{
		Project: h.Project.Name, SHA: h.SHA, Version: h.Version, Trigger: h.Trigger,
		Status: h.Status, FailureKind: h.FailureKind, FailedStep: h.FailedStep,
		StartedAt: h.StartedAt, FinishedAt: h.FinishedAt, Error: h.Error, Steps: h.Steps,
	}
}

func (d *Deployer) handOffDir() string { return filepath.Join(d.root, ".self-update") }

// The helper's variables holding KEY's value: the new version's, and the previous one's for a rollback.
const (
	SecretPrefix         = "HANDOFF_SECRET_"
	PreviousSecretPrefix = "HANDOFF_PREVIOUS_SECRET_"
)

// handOff writes the hand-off and starts the helper, passing it the secrets in its environment.
func (r *run) handOff(ctx context.Context, project compose.Project, dir string, secrets []string, out io.Writer) *failure {
	if r.prevEnv == nil && len(r.previous) > 0 && r.currentDir(project.Name) != "" {
		return transient(errors.New("the previous version's secrets couldn't be fetched from Cove, so a rollback couldn't start it again; the update is tried again later"))
	}
	image := ""
	for _, s := range project.Services {
		if s.Build {
			image = rollbackRef(s.ImageName(project.Name), r.req.SHA)
			break
		}
	}
	if image == "" {
		return permanent(errors.New("Lighthouse's compose file builds no image, so there's no new version to run the update helper from"))
	}

	now := time.Now()
	steps := append(append([]projects.Step{}, r.steps...), projects.Step{Name: StepHandOff, Status: projects.StepSucceeded,
		StartedAt: now, FinishedAt: now, Log: "handed off to the update helper (" + HelperName + ") running " + image + "\n"})
	h := HandOff{
		Project: r.req.Project, SHA: r.req.SHA, Version: r.req.Version, Trigger: r.req.Trigger,
		ComposeProject: project.Name, Dir: dir, Services: project.Services, Previous: r.previous,
		StartedAt: r.started, Steps: steps, Secrets: keyNames(secrets), PreviousSecrets: keyNames(r.prevEnv),
	}
	env := append(prefixed(SecretPrefix, secrets), prefixed(PreviousSecretPrefix, r.prevEnv)...)
	if err := os.MkdirAll(r.d.handOffDir(), 0o700); err != nil {
		return transient(fmt.Errorf("self-update: create %s: %w", r.d.handOffDir(), err))
	}
	h.Path = filepath.Join(r.d.handOffDir(), short12(r.req.SHA)+".json")
	if err := writeHandOff(h); err != nil {
		return transient(err)
	}

	if err := r.d.compose.RemoveContainer(ctx, HelperName); err != nil {
		fmt.Fprintf(out, "! remove the last update's helper: %v\n", err)
	}
	fmt.Fprintf(out, "starting the update helper (%s) from %s, with %d secrets and the previous version's %d\n",
		HelperName, image, len(h.Secrets), len(h.PreviousSecrets))
	err := r.d.compose.RunDetached(ctx, HelperName, image,
		[]string{"/var/run/docker.sock:/var/run/docker.sock", r.d.root + ":" + r.d.root}, env,
		[]string{"/lighthouse", "self-update", h.Path})
	if err != nil {
		os.Remove(h.Path)
		return transient(fmt.Errorf("self-update: start the helper: %w", err))
	}
	fmt.Fprintf(out, "handed off: the helper swaps Lighthouse now; \"docker logs -f %s\" shows how it goes\n", HelperName)
	return nil
}

// keyNames are the names in KEY=value lines.
func keyNames(env []string) []string {
	var names []string
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		names = append(names, key)
	}
	return names
}

// prefixed puts prefix before each KEY=value line's name.
func prefixed(prefix string, env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		out = append(out, prefix+kv)
	}
	return out
}

// handOffSecrets reads the named secrets from the helper's environment, as lines and values to hide.
func handOffSecrets(prefix string, names []string) ([]string, map[string]string, error) {
	env := []string{}
	values := map[string]string{}
	var missing []string
	for _, key := range names {
		v, ok := os.LookupEnv(prefix + key)
		if !ok {
			missing = append(missing, key)
			continue
		}
		env = append(env, key+"="+v)
		values[key] = v
	}
	if len(missing) > 0 {
		return nil, nil, fmt.Errorf("self-update: the helper wasn't given %s (expected as %s<key> in its environment), so nothing was swapped", strings.Join(missing, ", "), prefix)
	}
	return env, values, nil
}

func writeHandOff(h HandOff) error {
	data, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return fmt.Errorf("self-update: %w", err)
	}
	tmp := h.Path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("self-update: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, h.Path); err != nil {
		return fmt.Errorf("self-update: write %s: %w", h.Path, err)
	}
	return nil
}

func readHandOff(path string) (HandOff, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return HandOff{}, fmt.Errorf("self-update: read %s: %w", path, err)
	}
	var h HandOff
	if err := json.Unmarshal(data, &h); err != nil {
		return HandOff{}, fmt.Errorf("self-update: %s isn't a hand-off: %w", path, err)
	}
	h.Path = path
	return h, nil
}

// FinishHandOff is the helper's work: swap, verify, roll back if needed, record the result.
func (d *Deployer) FinishHandOff(ctx context.Context, path string) error {
	h, err := readHandOff(path)
	if err != nil {
		return err
	}
	if h.Done {
		return fmt.Errorf("self-update: %s is already finished (%s)", path, h.Status)
	}

	secrets, values, err := handOffSecrets(SecretPrefix, h.Secrets)
	var prevEnv []string
	var prevValues map[string]string
	if err == nil {
		prevEnv, prevValues, err = handOffSecrets(PreviousSecretPrefix, h.PreviousSecrets)
	}

	r := &run{d: d, req: Request{Project: h.Project, SHA: h.SHA, Version: h.Version, Trigger: h.Trigger},
		log: d.log.With("project", h.Project.Name, "sha", short(h.SHA)), previous: h.Previous,
		prevEnv: prevEnv, fallbackDir: h.Dir, fallbackEnv: secrets, scrub: newScrubber(values)}
	r.scrub.add(prevValues)
	project := compose.Project{Name: h.ComposeProject, Services: h.Services}
	env := append(append([]string{}, secrets...), deployVars(h.SHA, h.Version)...)

	res := Result{}
	fail := func(step string, f *failure) Result {
		res.Status, res.FailedStep, res.FailureKind = projects.StatusFailed, step, f.kind
		res.Err = fmt.Errorf("%s: %w", step, f.err)
		return res
	}
	if err != nil {
		res = fail(StepHandOff, transient(err))
	} else if f := r.step(ctx, StepSwap, func(ctx context.Context, out io.Writer) *failure {
		if err := d.compose.Up(ctx, h.Dir, project.Name, env, out); err != nil {
			return permanent(err)
		}
		return nil
	}); f != nil {
		res = r.rollBack(ctx, fail(StepSwap, f), project, h.Dir)
	} else if f := r.step(ctx, StepVerify, func(ctx context.Context, out io.Writer) *failure {
		return r.verify(ctx, project, out)
	}); f != nil {
		res = r.rollBack(ctx, fail(StepVerify, f), project, h.Dir)
	} else {
		r.step(ctx, StepCleanup, func(ctx context.Context, out io.Writer) *failure {
			r.cleanup(ctx, project, h.Dir, out)
			return nil
		})
		res.Status = projects.StatusSucceeded
	}

	h.Done, h.Status, h.FailureKind, h.FailedStep = true, res.Status, res.FailureKind, res.FailedStep
	if res.Err != nil {
		h.Error = projects.ErrorText(res.Err)
	}
	h.FinishedAt = time.Now()
	h.Steps = append(h.Steps, r.steps...)
	if err := writeHandOff(h); err != nil {
		return err
	}
	if res.Err != nil {
		return fmt.Errorf("self-update of %s: %w", short(h.SHA), res.Err)
	}
	return nil
}

// handOffs reads every hand-off file.
func (d *Deployer) handOffs() ([]HandOff, error) {
	entries, err := os.ReadDir(d.handOffDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("self-update: read %s: %w", d.handOffDir(), err)
	}
	var all []HandOff
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		h, err := readHandOff(filepath.Join(d.handOffDir(), e.Name()))
		if err != nil {
			d.log.Error("a self-update hand-off can't be read; it's left for a person to look at", "err", err)
			continue
		}
		all = append(all, h)
	}
	return all, nil
}

// HandOffs returns the self-updates ready to record: finished, or unfinished for too long.
func (d *Deployer) HandOffs() ([]HandOff, error) {
	all, err := d.handOffs()
	if err != nil {
		return nil, err
	}
	var ready []HandOff
	for _, h := range all {
		if !h.Done {
			if time.Since(h.StartedAt) < HandOffWait {
				continue
			}
			h.Done, h.Status, h.FailureKind, h.FailedStep = true, projects.StatusFailed, projects.FailureTransient, StepHandOff
			h.Error = fmt.Sprintf("the update helper didn't finish within %s; \"docker logs %s\" may say why", HandOffWait, HelperName)
			h.FinishedAt = time.Now()
		}
		ready = append(ready, h)
	}
	return ready, nil
}

// HandOffInProgress reports whether the helper is still working on the project.
func (d *Deployer) HandOffInProgress(project string) bool {
	all, err := d.handOffs()
	if err != nil {
		// Unknown counts as in progress: a deploy now could replace the helper's files.
		d.log.Warn("self-update hand-offs can't be read, so deploys wait", "err", err)
		return true
	}
	for _, h := range all {
		if !h.Done && strings.EqualFold(h.Project.Name, project) && time.Since(h.StartedAt) < HandOffWait {
			return true
		}
	}
	return false
}

// Forget deletes a recorded hand-off, and after a success the helper, whose configuration has the secrets.
func (d *Deployer) Forget(h HandOff) error {
	if err := os.Remove(h.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("self-update: remove %s: %w", h.Path, err)
	}
	if h.Status == projects.StatusSucceeded {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := d.compose.RemoveContainer(ctx, HelperName); err != nil {
			d.log.Warn("the update helper's container can't be removed; the next update removes it", "container", HelperName, "err", err)
		}
	}
	return nil
}

// The variables every deploy gives Compose, so a compose file can label what it runs.
const (
	DeployCommitVar  = "LIGHTHOUSE_DEPLOY_COMMIT"
	DeployVersionVar = compose.VersionVar
)

// deployVars are the deploy's variables: the commit, and the release or short commit.
func deployVars(sha string, version string) []string {
	if version == "" {
		version = short(sha)
	}
	return []string{DeployCommitVar + "=" + sha, DeployVersionVar + "=" + version}
}
