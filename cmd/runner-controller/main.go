// Command runner-controller starts ephemeral GitHub Actions runners on
// demand, from runner scale sets, as systemd units within one memory budget.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Infinite-Device-Labs/runner-controller/internal/actions"
	"github.com/Infinite-Device-Labs/runner-controller/internal/config"
	"github.com/Infinite-Device-Labs/runner-controller/internal/controller"
	"github.com/Infinite-Device-Labs/runner-controller/internal/systemd"
)

// version is set at release time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	configPath := flag.String("config", "/etc/runner-controller/config.toml", "the configuration file")
	check := flag.Bool("check", false, "check the configuration file and exit")
	logLevel := flag.String("log-level", "info", "debug, info, warn or error")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		fmt.Fprintf(os.Stderr, "runner-controller: -log-level: %v\n", err)
		os.Exit(2)
	}
	// journald adds the time.
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))

	if err := run(*configPath, *check, log); err != nil {
		log.Error("stopped", "error", err)
		os.Exit(1)
	}
}

func run(configPath string, check bool, log *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}

	clients := map[string]actions.Client{}
	for _, s := range cfg.ScaleSets {
		key, err := s.GitHubApp.PrivateKey()
		if err != nil {
			return fmt.Errorf("scale set %q: %w", s.Name, err)
		}
		clients[s.Name], err = actions.New(s.URL, actions.AppCredentials{
			ID:             string(s.GitHubApp.ID),
			InstallationID: s.GitHubApp.InstallationID,
			PrivateKey:     key,
		}, version, log)
		if err != nil {
			return fmt.Errorf("scale set %q: %w", s.Name, err)
		}
	}
	if check {
		if err := controller.CheckDirs(cfg); err != nil {
			return err
		}
		fmt.Println("the configuration is valid")
		return nil
	}

	c, err := controller.New(cfg, clients, systemd.Systemctl{}, log)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Info("starting", "version", version, "scale_sets", len(cfg.ScaleSets), "memory_budget", cfg.MemoryBudget)
	return c.Run(ctx)
}
