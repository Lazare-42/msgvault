package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "wealthfolio-sync:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath string
		dryRun     bool
	)

	flag.StringVar(&configPath, "config", "", "Path to wealthfolio sync TOML config")
	flag.BoolVar(&dryRun, "dry-run", false, "Print matching exports without writing files or running rsync")
	flag.Parse()

	if configPath == "" {
		return fmt.Errorf("--config is required")
	}

	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	state, err := loadState(cfg.StateFileAbs)
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	runner := newSyncRunner(cfg, state, dryRun)
	result, err := runner.run(ctx)
	if err != nil {
		return err
	}

	fmt.Printf(
		"Completed wealthfolio sync: messages=%d new_attachments=%d outbox=%s\n",
		result.MessagesScanned,
		result.AttachmentsNew,
		cfg.OutputDirAbs,
	)
	return nil
}
