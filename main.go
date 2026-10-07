package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		logLine(os.Stdout, "ERROR: Worker stopped: %s", logText(err.Error()))
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "config.json", "configuration file")
	once := flag.Bool("once", false, "run one reconciliation cycle")
	apply := flag.Bool("apply", false, "enable collection writes and configured acquisition (default is dry-run)")
	check := flag.Bool("check-config", false, "validate configuration without API calls")
	flag.Parse()
	config, interval, err := LoadConfig(*configPath)
	if err != nil {
		return err
	}
	for _, connection := range []Connection{config.Silo, config.Floppy} {
		if _, err := NewAPI(connection.URL, "", connection.ProfileID); err != nil {
			return err
		}
	}
	if *check {
		logLine(os.Stdout, "Configuration valid | %d lists | Interval: %s", len(config.Lists), interval)
		return nil
	}
	siloToken, err := readToken(config.Silo.TokenFile, "SILO_TOKEN")
	if err != nil {
		return fmt.Errorf("Silo credential: %w", err)
	}
	floppyToken, err := readToken(config.Floppy.TokenFile, "FLOPPY_TOKEN")
	if err != nil {
		return fmt.Errorf("Floppy credential: %w", err)
	}
	silo, _ := NewSiloAPI(config.Silo.URL, siloToken, config.Silo.ProfileID)
	floppy, _ := NewAPI(config.Floppy.URL, floppyToken, "")
	state, err := OpenState(config.StatePath)
	if err != nil {
		return err
	}
	defer state.Close()
	radarr, err := NewArr(config.Radarr, "movie", state)
	if err != nil {
		return err
	}
	sonarr, err := NewArr(config.Sonarr, "tv", state)
	if err != nil {
		return err
	}
	if os.Getenv("SILO_DATABASE_URL") == "" {
		return fmt.Errorf("SILO_DATABASE_URL is required for read-only matching")
	}
	database, err := sql.Open("pgx", os.Getenv("SILO_DATABASE_URL"))
	if err != nil {
		return fmt.Errorf("invalid read-only database configuration")
	}
	defer database.Close()
	database.SetMaxOpenConns(1)
	matcher := &DatabaseMatcher{DB: database}
	service := &Service{Silo: silo, Floppy: floppy, State: state, DryRun: !*apply, Lookup: matcher.Lookup, Radarr: radarr, Sonarr: sonarr}
	if config.AcquisitionMode == "vio" {
		service.Vio = &Vio{API: silo, State: state}
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	for {
		cycle, stop := context.WithTimeout(ctx, 2*time.Hour)
		err := service.Run(cycle, config.Lists)
		stop()
		if ctx.Err() != nil {
			return nil
		}
		if *once {
			return err
		}
		if err != nil {
			logLine(os.Stdout, "WARNING: Cycle incomplete; retrying at next interval")
		}
		logLine(os.Stdout, "Next sync in %s | %s", interval, time.Now().Add(interval).Format(time.RFC3339))
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}
