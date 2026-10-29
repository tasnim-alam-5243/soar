// Command soar is the single binary for every SOAR role; the subcommand picks the role.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/yourorg/soar/internal/api"
	"github.com/yourorg/soar/internal/collector"
	"github.com/yourorg/soar/internal/config"
	"github.com/yourorg/soar/internal/ingest"
	"github.com/yourorg/soar/internal/obs"
	"github.com/yourorg/soar/internal/orchestrator"
	"github.com/yourorg/soar/internal/processor"
	"github.com/yourorg/soar/internal/scheduler"
	"github.com/yourorg/soar/internal/service"
	"github.com/yourorg/soar/internal/worker"
)

type runFunc func(context.Context, config.Config, *slog.Logger) error

var commands = map[string]struct {
	run  runFunc
	help string
}{
	"api":          {api.Run, "REST API, auth, CRUD, search"},
	"ingest":       {ingest.Run, "HTTP push receiver (never waits on OpenSearch)"},
	"collector":    {collector.Run, "pull and stream adapters; owns cursors"},
	"processor":    {processor.Run, "consume EVENTS: dedup, bulk-write, match triggers"},
	"orchestrator": {orchestrator.Run, "advance executions"},
	"worker":       {worker.Run, "run actions, write step results"},
	"scheduler":    {scheduler.Run, "cron, pollers, timers, recovery (leader-elected)"},
	"migrate":      {service.Migrate, "index templates and migrations (run once, then exit)"},
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		usage(stderr)
		return 2
	}
	switch cmd := args[0]; cmd {
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	case "version", "--version":
		fmt.Fprintf(stdout, "soar %s (%s)\n", obs.Version, obs.Commit)
		return 0
	case "healthcheck":
		return healthcheck(args[1:], stdout, stderr)
	default:
		c, ok := commands[cmd]
		if !ok {
			fmt.Fprintf(stderr, "unknown command %q\n\n", cmd)
			usage(stderr)
			return 2
		}
		return serve(cmd, c.run, stdout)
	}
}

func serve(name string, fn runFunc, out io.Writer) int {
	cfg, err := config.FromEnv()
	log := obs.NewLogger(out, cfg.LogLevel, name)
	if err != nil {
		var ve *config.ValidationError
		if errors.As(err, &ve) {
			log.Error("invalid configuration, refusing to start", "problems", ve.Problems)
		} else {
			log.Error("cannot load configuration", "error", err.Error())
		}
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := fn(ctx, cfg, log); err != nil {
		log.Error("fatal", "error", err.Error())
		return 1
	}
	log.Info("exited cleanly")
	return 0
}

// healthcheck lets distroless containers (no shell, no curl) probe themselves:
//
//	soar healthcheck [url]   # default http://127.0.0.1:8080/readyz
func healthcheck(args []string, stdout, stderr io.Writer) int {
	url := "http://127.0.0.1:8080/readyz"
	if len(args) > 0 {
		url = args[0]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(stderr, "unhealthy: HTTP %d\n", resp.StatusCode)
		return 1
	}
	fmt.Fprintln(stdout, "ok")
	return 0
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: soar <command>\n\ncommands:")
	names := make([]string, 0, len(commands))
	for n := range commands {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(w, "  %-13s %s\n", n, commands[n].help)
	}
	fmt.Fprintln(w, "  healthcheck   probe a URL and exit 0 if HTTP 200 (for container health checks)")
	fmt.Fprintln(w, "  version       print version")
	fmt.Fprintln(w, "\nconfiguration is read from SOAR_* environment variables (see .env.example)")
}
