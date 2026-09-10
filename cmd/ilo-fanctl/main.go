// Command ilo-fanctl raises fan floors on an HPE iLO 4 BMC from host-side
// temperatures.
//
// It must run on the machine that physically owns the drives and the IPMI KCS
// interface. On a virtualised host that means the hypervisor, not a guest: a
// VM has neither the raw block devices smartctl needs nor /dev/ipmi0.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/alekc/ilo-fanctl/internal/config"
	"github.com/alekc/ilo-fanctl/internal/controller"
	"github.com/alekc/ilo-fanctl/internal/metrics"
	"github.com/alekc/ilo-fanctl/internal/tui"
)

// version is set at build time with -ldflags "-X main.version=...". It has to
// stay a plain string constant assignment, because -X only writes to one of
// those, which is why the fallback below is in init rather than here.
var version = "dev"

func init() { version = resolveVersion(version, debug.ReadBuildInfo) }

// resolveVersion fills in a version for builds that carry no ldflags.
//
// A binary from `go install <module>@<version>` gets none, so without this it
// reports "dev", and so does its ilo_fanctl_build_info series, which leaves
// every go-install host indistinguishable from every other one in the metric
// that exists to tell them apart. The toolchain does record the module
// version for those builds, and it is the real tag.
//
// A plain `go build` in a checkout records "(devel)" instead, which says no
// more than "dev" already does, so that case keeps whatever it had.
//
// read is a parameter only so the tests can supply the build info that a test
// binary cannot otherwise have; production passes debug.ReadBuildInfo.
func resolveVersion(current string, read func() (*debug.BuildInfo, bool)) string {
	if current != "dev" {
		return current
	}
	info, ok := read()
	if !ok || info == nil || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return current
	}
	return info.Main.Version
}

func main() {
	// The subcommand comes first, as it does for go itself, so the flag set
	// below stays a single flat one shared by both modes.
	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 && args[0] == "tui" {
		cmd, args = args[0], args[1:]
	}

	fs := flag.NewFlagSet("ilo-fanctl", flag.ExitOnError)
	var (
		cfgPath   = fs.String("config", "/etc/ilo-fanctl/config.yaml", "path to the YAML config")
		dryRun    = fs.Bool("dry-run", false, "read and decide as normal, but never write a fan floor")
		checkOnly = fs.Bool("check", false, "validate the config and exit")
		logLevel  = fs.String("log-level", "info", "debug, info, warn or error")
		showVer   = fs.Bool("version", false, "print the version and exit")
		view      = fs.Bool("view", false, "tui: attach read only, never take the control loop")
	)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), `ilo-fanctl [tui] [flags]

  (no subcommand)  run the control loop as a daemon
  tui              run in the foreground with a live display. If nothing else
                   holds the listen address, this process takes the control
                   loop; if something does, it attaches to that daemon's
                   /metrics read only.

`)
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)

	if *showVer {
		fmt.Println("ilo-fanctl", version)
		return
	}

	level := parseLevel(*logLevel)
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config is not usable: %s: %v\n", *cfgPath, err)
		os.Exit(1)
	}
	if *checkOnly {
		fmt.Printf("%s: ok (%d fans, %d sensor groups, %d curves)\n",
			*cfgPath, len(cfg.Fans), len(cfg.Sensors), len(cfg.Curves))
		return
	}

	if cmd == "tui" {
		if err := runTUI(*cfgPath, cfg, level, *dryRun, *view); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	runDaemon(*cfgPath, cfg, level, *dryRun)
}

func runDaemon(cfgPath string, cfg *config.Config, level slog.Level, dryRun bool) {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	m := metrics.New(version)
	ctrl := controller.New(cfgPath, cfg, log, m)
	ctrl.SetDryRun(dryRun)
	if dryRun {
		log.Warn("dry run: fan floors will be computed and logged but never written")
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           exporter(m),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Info("metrics listening", "addr", cfg.Listen, "path", "/metrics")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			// A dead exporter is not a reason to stop cooling the machine, so
			// this is loud but not fatal.
			log.Error("metrics listener stopped", "error", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// SIGHUP forces an immediate reload. The controller also polls the file
	// on its own, so this is for when you want the change to land now rather
	// than within the poll interval.
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			log.Info("SIGHUP received, reloading config")
			if err := ctrl.Reload(); err != nil {
				log.Error("reload failed, the previous config is still running", "error", err)
			}
		}
	}()

	log.Info("starting", "version", version, "config", cfgPath,
		"interval", cfg.Interval.Duration, "bmc", cfg.ILO.Host)

	err := ctrl.Run(ctx)

	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)

	if err != nil && !errors.Is(err, context.Canceled) {
		log.Error("exiting on error", "error", err)
		os.Exit(1)
	}
	log.Info("stopped")
}

// runTUI runs the foreground display, deciding first whether it is allowed to
// drive anything.
//
// The listen socket is the mutual exclusion. Binding it is what makes this
// process the controller, so two copies started by mistake can never both be
// writing fan floors to the same BMC: the second one loses the bind, attaches
// to the first, and says so in its header.
func runTUI(cfgPath string, cfg *config.Config, level slog.Level, dryRun, view bool) error {
	sink := tui.NewLogSink(500, level)
	log := slog.New(sink)

	var ln net.Listener
	if !view {
		var err error
		if ln, err = net.Listen("tcp", cfg.Listen); err != nil {
			log.Info("the listen address is already taken, attaching read only",
				"addr", cfg.Listen, "error", err)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if ln == nil {
		app := tui.NewApp(cfg, sink, nil, tui.NewScraper(cfg.Listen, cfg))
		return app.Run(ctx)
	}

	m := metrics.New(version)
	ctrl := controller.New(cfgPath, cfg, log, m)
	ctrl.SetDryRun(dryRun)
	app := tui.NewApp(cfg, sink, ctrl, nil)
	ctrl.Observer = app.Publish

	srv := &http.Server{Handler: exporter(m), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("metrics listener stopped", "error", err)
		}
	}()
	log.Info("starting", "version", version, "config", cfgPath,
		"interval", cfg.Interval.Duration, "bmc", cfg.ILO.Host, "metrics", cfg.Listen)

	// The loop's context is separate from the signal one so that quitting the
	// UI with q cancels it too. Leaving without cancelling would skip the
	// wind-down and leave the fans pinned at whatever floor was last set.
	loopCtx, endLoop := context.WithCancel(ctx)
	defer endLoop()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = ctrl.Run(loopCtx)
	}()

	uiErr := app.Run(ctx)

	// The terminal is ours again, so the wind-down should be visible.
	sink.Detach(os.Stderr)
	endLoop()
	select {
	case <-done:
	case <-time.After(45 * time.Second):
		log.Error("the control loop did not finish winding the fans down")
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	return uiErr
}

func exporter(m *metrics.Metrics) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", m.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	return mux
}

func parseLevel(s string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return slog.LevelInfo
	}
	return l
}
