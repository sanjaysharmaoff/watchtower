// Command watchtowerd is the WatchTower service entrypoint.
//
// It does four things and nothing else: load configuration, build the app, run it,
// and translate a signal into a graceful shutdown.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/sanjaysharmaoff/watchtower/internal/bootstrap"
	"github.com/sanjaysharmaoff/watchtower/internal/config"
)

func main() {
	envFile := flag.String("env-file", ".env", "path to a KEY=VALUE file loaded before the environment is read")
	showVersion := flag.Bool("version", false, "print the version and exit")
	job := flag.String("job", "", "run one job (digest|maintenance) and exit instead of serving")
	flag.Parse()

	if *showVersion {
		fmt.Println("watchtower", bootstrap.Version)
		return
	}

	if err := config.LoadDotEnv(*envFile); err != nil {
		fmt.Fprintf(os.Stderr, "watchtower: read %s: %v\n", *envFile, err)
		os.Exit(1)
	}

	app, err := bootstrap.New(config.Load())
	if err != nil {
		fmt.Fprintf(os.Stderr, "watchtower: startup failed: %v\n", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	run := app.Run
	if *job != "" {
		run = func(ctx context.Context) error { return app.RunOnce(ctx, *job) }
	}

	if err := run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "watchtower: %v\n", err)
		os.Exit(1)
	}
}
