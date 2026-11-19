package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/lsariol/blackboxlog"
	"github.com/lsariol/coveclient"
	"github.com/lsariol/lighthouse"
	"github.com/lsariol/lighthouse/internal/api"
	"github.com/lsariol/lighthouse/internal/cli"
	"github.com/lsariol/lighthouse/internal/compose"
	"github.com/lsariol/lighthouse/internal/config"
	"github.com/lsariol/lighthouse/internal/control"
	"github.com/lsariol/lighthouse/internal/cove"
	"github.com/lsariol/lighthouse/internal/daemon"
	"github.com/lsariol/lighthouse/internal/database"
	"github.com/lsariol/lighthouse/internal/deploy"
	"github.com/lsariol/lighthouse/internal/docker"
	"github.com/lsariol/lighthouse/internal/github"
	"github.com/lsariol/lighthouse/internal/orchestrator"
	"github.com/lsariol/lighthouse/internal/policy"
)

// retryEvery is how long startup waits before trying an unreachable Cove or database again.
const retryEvery = 15 * time.Second

// shutdownWait is how long stopping waits for a deploy in progress; keep it under stop_grace_period.
const shutdownWait = 100 * time.Second

func main() {
	args := os.Args[1:]

	mode := ""
	if len(args) > 0 {
		mode = args[0]
	}

	switch mode {
	case "":
		runServe(true)
	case "serve":
		runServe(false)
	case "api":
		runAPI()
	case "-h", "--help":
		printUsage(os.Stdout)
	case "shell":
		runShell()
	case "migrate":
		runMigrate(args[1:])
	case "version", "--version":
		fmt.Println(buildVersion(loadConfig()))
	case "health":
		os.Exit(runHealth(args[1:]))
	case "self-update":
		os.Exit(runSelfUpdate(args[1:]))
	default:
		os.Exit(runCommand(args))
	}
}

func printUsage(w io.Writer) {
	fmt.Fprint(w, `Usage:
  lighthouse                    Run Lighthouse with its CLI on this terminal (local use)
  lighthouse serve              Run Lighthouse only (what Docker runs)
  lighthouse api                Run lighthouse-api, the read-only API for other projects
  lighthouse shell              Open the CLI of a running Lighthouse
  lighthouse <command> [args]   Run one CLI command and exit
  lighthouse migrate [status|up]
                                Show or apply database migrations
  lighthouse version            Print the version
  lighthouse health [api]       Exit 0 if the daemon (or the API) answers (the healthchecks)
  lighthouse self-update <file> The update helper's work (Lighthouse starts it itself)

In Docker: docker exec -it lighthouse /lighthouse shell
       or: docker exec lighthouse /lighthouse status
Run "lighthouse help" to list the CLI commands.
`)
}

// runServe runs the daemon until SIGINT or SIGTERM (docker stop).
func runServe(withShell bool) {
	// After Load: a local .env can set APP_ENV, which picks the log format.
	cfg, err := config.Load()
	log := newLogger(withShell)
	slog.SetDefault(log)
	if err != nil {
		fatalLog(log, err)
	}
	if err := cfg.ValidateServe(); err != nil {
		fatalLog(log, err)
	}

	version := buildVersion(cfg)
	log.Info("Lighthouse starting")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dockerClient, err := docker.New()
	if err != nil {
		fatalLog(log, err)
	}
	defer dockerClient.Close()

	d := daemon.New(cfg, version, dockerClient, compose.Runner{})

	controlDone := make(chan error, 1)
	go func() { controlDone <- control.Serve(ctx, cfg.ControlSocket, d, log.With("component", "control")) }()

	if withShell {
		shell := cli.New(d, cli.Options{Env: cfg.Env, Embedded: true})
		go shell.Run(ctx, stop)
	}

	loopDone := make(chan struct{})
	go func() {
		defer close(loopDone)

		db, orch, err := start(ctx, cfg, d, dockerClient, log)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			fatalServe(cfg, log, err)
		}
		defer db.Close()

		orch.Run(ctx, cfg.PollInterval)
	}()

	select {
	case err := <-controlDone:
		if err != nil && ctx.Err() == nil {
			fatalLog(log, err)
		}
	case <-ctx.Done():
	}

	log.Info("Lighthouse stopping")
	select {
	case <-loopDone:
	case <-time.After(shutdownWait):
		log.Warn("a deploy was still running at shutdown")
	}
	<-controlDone
	log.Info("Lighthouse stopped")
}

// start connects Cove and the database, migrates it, and hands the daemon its store and orchestrator.
func start(ctx context.Context, cfg config.Config, d *daemon.Daemon, dockerClient *docker.Client, log *slog.Logger) (*database.Database, *orchestrator.Orchestrator, error) {
	rules, err := policy.Parse(lighthouse.PolicyFile)
	if err != nil {
		return nil, nil, err
	}
	if len(rules.Exceptions) > 0 {
		log.Info("deploy rule exceptions loaded", "exceptions", len(rules.Exceptions))
	}
	if _, err := os.Stat(cfg.StoragePath); err != nil {
		log.Warn("STORAGE_PATH isn't readable, so symlinks in the projects' data folders can't be checked: mount it read-only at the same path",
			"path", cfg.StoragePath, "err", err)
	}

	coveClient := coveclient.New(cfg.CoveURL, "")

	d.SetPhase(daemon.PhaseWaiting)
	if err := cove.Connect(ctx, coveClient, cfg.CoveTokenPath, retryEvery, log.With("component", "cove")); err != nil {
		return nil, nil, err
	}
	secrets, err := cove.ReadSecrets(ctx, coveClient)
	if err != nil {
		return nil, nil, err
	}
	log.Info("secrets loaded from Cove")

	db, err := openDatabase(ctx, d, secrets, log)
	if err != nil {
		return nil, nil, err
	}

	self, err := dockerClient.SelfProject(ctx)
	if err != nil {
		log.Warn("self-update is off: Lighthouse can't find its own container", "err", err)
	}
	deployer := deploy.New(compose.Runner{}, dockerClient, github.Client{}, coveClient, deploy.Options{
		Self:    self,
		Root:    cfg.StagingPath,
		Storage: cfg.StoragePath,
		Backups: cfg.BackupPath,
		Policy:  rules,
		Output:  os.Stderr,
		Log:     log,
	})
	if self != "" {
		log.Info("self-update is on")
	}
	commits := github.Client{HTTP: &http.Client{Timeout: 30 * time.Second}}
	orch := orchestrator.New(db, commits, deployer, dockerClient, secrets.GitHubToken, log)
	if cfg.GitHubAppID == "" {
		orch.SetPublisher(nil, "GITHUB_APP_ID isn't set (README: \"Publishing GitHub Releases\")")
	} else {
		orch.SetPublisher(&github.App{ID: cfg.GitHubAppID, HTTP: commits.HTTP,
			Key: func(ctx context.Context) ([]byte, error) { return cove.AppKey(ctx, coveClient) }}, "")
		log.Info("publishing GitHub Releases is on", "app_id", cfg.GitHubAppID)
	}
	d.Ready(db, orch, db)
	orch.RecordHandOffs(ctx)
	go orch.RunPublisher(ctx)

	list, err := db.List(ctx)
	if err != nil {
		log.Warn("the projects can't be counted", "err", err)
	}
	log.Info("Lighthouse ready", "projects", len(list), "poll_interval", cfg.PollInterval.String(), "control_socket", cfg.ControlSocket)
	return db, orch, nil
}

// openDatabase migrates as the migrator, then connects as the app and checks the schema.
func openDatabase(ctx context.Context, d *daemon.Daemon, secrets cove.Secrets, log *slog.Logger) (*database.Database, error) {
	log = log.With("component", "database")
	for {
		d.SetPhase(daemon.PhaseMigrating)
		err := database.Migrate(ctx, secrets.MigratorDatabaseURL, log)
		if err == nil {
			var db *database.Database
			db, err = database.Connect(ctx, secrets.DatabaseURL)
			if err == nil {
				if err := db.CheckSchemaVersion(ctx); err != nil {
					db.Close()
					return nil, err
				}
				return db, nil
			}
		}
		if !database.IsUnreachable(err) {
			return nil, err
		}

		d.SetPhase(daemon.PhaseDatabase)
		log.Warn("waiting for the database to be reachable", "err", err)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(retryEvery):
		}
	}
}

// runShell runs the interactive CLI next to a running daemon.
func runShell() {
	cfg := loadConfig()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()

	shell := cli.New(control.NewClient(cfg.ControlSocket), cli.Options{Env: cfg.Env})
	shell.Run(ctx, stop)
}

// runHealth is the containers' healthcheck: 0 once the daemon, or with "api" the API, answers.
func runHealth(args []string) int {
	if len(args) > 1 || (len(args) == 1 && args[0] != "api") {
		fmt.Fprintln(os.Stderr, "Usage: lighthouse health [api]")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if len(args) == 1 {
		if err := apiHealthy(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "lighthouse health: %v\n", err)
			return 1
		}
		fmt.Println("ok")
		return 0
	}
	cfg := loadConfig()
	s, err := control.NewClient(cfg.ControlSocket).Status(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lighthouse health: %v\n", err)
		return 1
	}
	fmt.Println(s.Phase)
	return 0
}

// apiHealthy is nil while lighthouse-api answers and reaches the database.
func apiHealthy(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/healthz", api.Port), nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("the API doesn't answer on port %d: %w", api.Port, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the API answers %s: it can't reach the database yet (see docker logs lighthouse-api)", resp.Status)
	}
	return nil
}

// runAPI runs lighthouse-api until SIGINT or SIGTERM.
func runAPI() {
	cfg, err := config.Load()
	log := newLogger(false)
	slog.SetDefault(log)
	if err != nil {
		fatalLog(log, err)
	}
	if err := cfg.ValidateAPI(); err != nil {
		fatalLog(log, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv, err := api.New(api.Options{
		Version: buildVersion(cfg),
		Getenv:  config.Lookup,
		Log:     log,
		OpenReader: func(ctx context.Context) (api.Reader, error) {
			return database.ConnectReader(ctx, cfg.ReaderURL)
		},
	})
	if err != nil {
		fatalLog(log, err)
	}
	if err := srv.Serve(ctx, fmt.Sprintf(":%d", api.Port)); err != nil {
		fatalLog(log, err)
	}
	log.Info("API stopped")
}

// runSelfUpdate is the update helper's work (internal/deploy/selfupdate.go).
func runSelfUpdate(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "Usage: lighthouse self-update <hand-off file>")
		return 2
	}
	log := blackboxlog.New(blackboxlog.Options{})
	slog.SetDefault(log)
	dockerClient, err := docker.New()
	if err != nil {
		fatalLog(log, err)
	}
	defer dockerClient.Close()

	deployer := deploy.New(compose.Runner{}, dockerClient, nil, nil, deploy.Options{
		Root:   filepath.Dir(filepath.Dir(args[0])),
		Output: os.Stderr,
		Log:    log,
	})
	if err := deployer.FinishHandOff(context.Background(), args[0]); err != nil {
		log.Error("self-update failed", "err", err)
		return 1
	}
	log.Info("self-update finished: the new Lighthouse is healthy")
	return 0
}

// runCommand runs one CLI command and returns the exit status.
func runCommand(args []string) int {
	cfg := loadConfig()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shell := cli.New(control.NewClient(cfg.ControlSocket), cli.Options{Env: cfg.Env})
	if err := shell.Exec(ctx, args); err != nil {
		cli.Report(err)
		return 1
	}
	return 0
}

// adminSecrets reads Lighthouse's secrets from Cove for `lighthouse migrate`.
func adminSecrets(ctx context.Context, cfg config.Config, log *slog.Logger) cove.Secrets {
	if err := cfg.ValidateCove(); err != nil {
		fatal(err)
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	c := coveclient.New(cfg.CoveURL, "")
	if err := cove.Connect(ctx, c, cfg.CoveTokenPath, 5*time.Second, log); err != nil {
		fatal(fmt.Errorf("connecting to Cove: %w", err))
	}
	secrets, err := cove.ReadSecrets(ctx, c)
	if err != nil {
		fatal(err)
	}
	return secrets
}

// runMigrate handles `lighthouse migrate [status|up]`, as the migrator.
func runMigrate(args []string) {
	command := "status"
	if len(args) > 0 {
		command = args[0]
	}
	if len(args) > 1 || (command != "status" && command != "up") {
		fmt.Fprintln(os.Stderr, "! Usage: lighthouse migrate [status|up]")
		os.Exit(1)
	}

	cfg := loadConfig()
	log := blackboxlog.New(blackboxlog.Options{Format: blackboxlog.Text})

	ctx := context.Background()
	secrets := adminSecrets(ctx, cfg, log)

	var err error
	if command == "up" {
		err = database.Migrate(ctx, secrets.MigratorDatabaseURL, log)
	} else {
		err = database.PrintMigrationStatus(ctx, secrets.MigratorDatabaseURL, os.Stdout)
	}
	if err != nil {
		fatal(err)
	}
}

func loadConfig() config.Config {
	cfg, err := config.Load()
	if err != nil {
		fatal(err)
	}
	return cfg
}

// buildVersion returns LIGHTHOUSE_VERSION (set in docker-compose.yml).
func buildVersion(cfg config.Config) string {
	if cfg.Version != "" {
		return cfg.Version
	}

	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" && len(setting.Value) >= 7 {
				return "dev-" + setting.Value[:7]
			}
		}
	}
	return "dev"
}

// newLogger returns Blackbox's format, or readable text when it shares a terminal with the CLI.
func newLogger(terminal bool) *slog.Logger {
	if terminal {
		return blackboxlog.New(blackboxlog.Options{Format: blackboxlog.Text})
	}
	return blackboxlog.New(blackboxlog.Options{})
}

// fatal prints an error and exits, for commands run by hand.
func fatal(err error) {
	fmt.Fprintf(os.Stderr, "lighthouse: %v\n", err)
	os.Exit(1)
}

// fatalLog is fatal for what runs under Docker: the reason is logged at ERROR.
func fatalLog(log *slog.Logger, err error) {
	log.Error("Lighthouse can't run", "err", err)
	os.Exit(1)
}

// fatalServe is fatalLog that also removes the control socket, so the next start doesn't find it.
func fatalServe(cfg config.Config, log *slog.Logger, err error) {
	os.Remove(cfg.ControlSocket)
	fatalLog(log, err)
}
