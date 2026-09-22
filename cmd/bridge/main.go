// Command bridge is the XMBridge daemon: a single process that mirrors your
// own posts between Mastodon, Bluesky, and X.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/andrinoff/xmbbridge/internal/config"
	"github.com/andrinoff/xmbbridge/internal/feed"
	"github.com/andrinoff/xmbbridge/internal/model"
	"github.com/andrinoff/xmbbridge/internal/platform/bluesky"
	"github.com/andrinoff/xmbbridge/internal/platform/mastodon"
	"github.com/andrinoff/xmbbridge/internal/platform/twitter"
	"github.com/andrinoff/xmbbridge/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "bridge: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath = flag.String("config", "config.yaml", "path to the YAML configuration file")
		check      = flag.Bool("check", false, "validate the configuration and credentials, then exit")
		logLevel   = flag.String("log-level", "", "override the configured log level (debug, info, warn, error)")
	)
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	level := *logLevel
	if level == "" {
		level = cfg.LogLevel
	}
	logger := setupLogger(level)

	st, err := store.Open(cfg.Storage)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	sources, targets, err := buildPlatforms(ctx, cfg, st, logger)
	if err != nil {
		return err
	}

	if stats, err := st.Stats(ctx); err == nil {
		logger.Info("state store ready", "bridged", stats.Bridged, "pending", stats.Pending, "failed", stats.Failed)
	}

	// checkable is implemented by adapters that can verify credentials
	// without starting to bridge.
	type checkable interface {
		Check(ctx context.Context) error
	}

	if *check {
		for _, source := range sources {
			if c, ok := source.(checkable); ok {
				if err := c.Check(ctx); err != nil {
					return err
				}
			}
		}
		logger.Info("configuration and credentials OK")
		return nil
	}

	engine := feed.New(cfg, st, logger, sources, targets)
	logRoutes(logger, cfg.EnabledPlatforms(), engine.Routes())

	logger.Info("bridge running", "sources", sourceNames(sources))
	err = engine.Run(ctx)
	logger.Info("bridge stopped")
	return err
}

// buildPlatforms constructs a listener per enabled platform and a writer per
// enabled writable platform.
func buildPlatforms(ctx context.Context, cfg *config.Config, st *store.Store, logger *slog.Logger) ([]feed.Source, []feed.Target, error) {
	var sources []feed.Source
	var targets []feed.Target

	if cfg.Platforms.Mastodon.Enabled {
		adapter, err := mastodon.New(mastodon.Options{
			Config:       cfg.Platforms.Mastodon,
			PollInterval: cfg.PollInterval.D(),
			Backfill:     cfg.Backfill,
			State:        st,
			Logger:       logger.With("component", "mastodon"),
		})
		if err != nil {
			return nil, nil, fmt.Errorf("mastodon: %w", err)
		}
		sources = append(sources, adapter)
		targets = append(targets, adapter)
	}

	if cfg.Platforms.Bluesky.Enabled {
		adapter, err := bluesky.New(ctx, bluesky.Options{
			Config:   cfg.Platforms.Bluesky,
			Backfill: cfg.Backfill,
			State:    st,
			Logger:   logger.With("component", "bluesky"),
		})
		if err != nil {
			return nil, nil, fmt.Errorf("bluesky: %w", err)
		}
		sources = append(sources, adapter)
		targets = append(targets, adapter)
	}

	if cfg.Platforms.Twitter.Enabled {
		adapter, err := twitter.New(twitter.Options{
			Config:       cfg.Platforms.Twitter,
			PollInterval: cfg.TwitterPollInterval.D(),
			Backfill:     cfg.Backfill,
			State:        st,
			Logger:       logger.With("component", "twitter"),
		})
		if err != nil {
			return nil, nil, fmt.Errorf("twitter: %w", err)
		}
		sources = append(sources, adapter)
		// X is read-only: it is a source, never a target.
	}

	return sources, targets, nil
}

func logRoutes(logger *slog.Logger, enabled []model.Platform, routes map[model.Platform][]model.Platform) {
	for _, origin := range enabled {
		targets := routes[origin]
		if len(targets) == 0 {
			continue
		}
		names := make([]string, len(targets))
		for i, target := range targets {
			names[i] = target.String()
		}
		logger.Info("route", "from", origin.String(), "to", strings.Join(names, ", "))
	}
}

func sourceNames(sources []feed.Source) []string {
	names := make([]string, len(sources))
	for i, source := range sources {
		names[i] = source.Name().String()
	}
	return names
}

func setupLogger(level string) *slog.Logger {
	var parsed slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		parsed = slog.LevelDebug
	case "warn", "warning":
		parsed = slog.LevelWarn
	case "error":
		parsed = slog.LevelError
	case "", "info":
		parsed = slog.LevelInfo
	default:
		parsed = slog.LevelInfo
		fmt.Fprintf(os.Stderr, "bridge: unrecognised log level %q, using info\n", level)
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: parsed}))
}
