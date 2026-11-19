package lighthouse

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/lsariol/lighthouse/internal/compose"
	"github.com/lsariol/lighthouse/internal/policy"
	"github.com/lsariol/lighthouse/internal/settings"
)

// The policy.json that ships must load, or Lighthouse won't start.
func TestPolicyFileLoads(t *testing.T) {
	if _, err := policy.Parse(PolicyFile); err != nil {
		t.Fatal(err)
	}
}

// Lighthouse's own compose file passes the rules with its exceptions, and lighthouse-api stays read-only and unprivileged.
func TestOwnComposeFileDeploys(t *testing.T) {
	if err := exec.Command("docker", "compose", "version").Run(); err != nil {
		t.Skip("docker compose isn't installed")
	}
	ctx := context.Background()
	dir, _ := os.Getwd()
	r := compose.Runner{}
	p, err := r.Inspect(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	vars, err := r.Variables(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	var secrets []string
	for _, v := range vars {
		if v.Default == "" {
			secrets = append(secrets, v.Name)
		}
	}
	slices.Sort(secrets)
	if want := []string{"LIGHTHOUSE_DATABASE_READER_URL", "SHARED_LIGHTHOUSE_VISTA_TOKEN"}; !slices.Equal(secrets, want) {
		t.Errorf("docker-compose.yml takes %v from Cove, want %v (lighthouse-api's only; the daemon reads its own)", secrets, want)
	}

	rules, _ := policy.Parse(PolicyFile)
	findings, err := policy.Check(policy.Input{Project: p.Name, Config: p.Config, Dir: dir, Storage: "/srv/server/storage",
		Resolve: func(path string) string { return path }})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range rules.Apply(p.Name, findings) {
		t.Errorf("refused: %s", f)
	}

	s, err := settings.FromConfig(p.Config)
	if err != nil || !s.Releases() || s.Tier != settings.TierInfra {
		t.Errorf("x-lighthouse = %+v, %v; want releases, infra", s, err)
	}
	if !strings.Contains(string(p.Config), `"/lighthouse"`) {
		t.Error("no healthcheck running /lighthouse health")
	}

	var cfg struct {
		Services map[string]struct {
			User        string          `json:"user"`
			ReadOnly    bool            `json:"read_only"`
			CapDrop     []string        `json:"cap_drop"`
			SecurityOpt []string        `json:"security_opt"`
			MemLimit    any             `json:"mem_limit"`
			Volumes     []any           `json:"volumes"`
			Environment json.RawMessage `json:"environment"`
		} `json:"services"`
	}
	if err := json.Unmarshal(p.Config, &cfg); err != nil {
		t.Fatal(err)
	}
	a, ok := cfg.Services["lighthouse-api"]
	switch {
	case !ok:
		t.Fatal("no lighthouse-api service")
	case a.User != "10001:10001" || !a.ReadOnly || !slices.Equal(a.CapDrop, []string{"ALL"}) ||
		!slices.Contains(a.SecurityOpt, "no-new-privileges:true") || a.MemLimit == nil:
		t.Errorf("lighthouse-api isn't hardened: %+v", a)
	case len(a.Volumes) > 0:
		t.Errorf("lighthouse-api mounts %v; it needs nothing on the host", a.Volumes)
	}
	if env := string(cfg.Services["lighthouse"].Environment); strings.Contains(env, "LIGHTHOUSE_DATABASE_READER_URL") || strings.Contains(env, "SHARED_LIGHTHOUSE_VISTA_TOKEN") {
		t.Error("the lighthouse service is given lighthouse-api's secrets")
	}
}
