// Command repoman serves a web dashboard for locally checked-out git
// repositories.
//
// It is single-tenant by design: one passphrase, no user accounts, no
// database. Run it behind a VPN and a reverse proxy that terminates TLS.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jbain/repo-man/internal/actions"
	"github.com/jbain/repo-man/internal/auth"
	"github.com/jbain/repo-man/internal/config"
	"github.com/jbain/repo-man/internal/github"
	"github.com/jbain/repo-man/internal/index"
	"github.com/jbain/repo-man/internal/jobs"
	"github.com/jbain/repo-man/internal/web"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		// -h has already printed usage; exiting zero keeps it from looking
		// like a failure.
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "repoman:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	cfg, err := config.Load(args, os.Stderr)
	if err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel()}))
	slog.SetDefault(log)

	// Signals cancel the root context, which unwinds the collection loops, the
	// in-flight jobs, and finally the HTTP server.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The provider client is optional: without a usable gh CLI the dashboard
	// still shows every local checkout, it just cannot show un-cloned repos.
	var lister index.Lister
	if len(cfg.Owners) > 0 {
		gh := github.New("")
		if err := gh.Check(ctx); err != nil {
			log.Warn("github listings disabled", "err", err)
		} else {
			lister = gh
		}
	}

	ix := index.New(cfg, lister, log)
	reg := jobs.New(jobs.Options{Base: ctx, OnDone: ix.ScanNow})
	act := actions.New(cfg.Root, reg, ix.ScanNow, log)
	authn := auth.New(auth.Options{
		Passphrase:   cfg.Passphrase,
		SecureCookie: cfg.SecureCookie,
		TrustProxyIP: cfg.TrustProxyIP,
	})
	if !authn.Enabled() {
		log.Warn("REPOMAN_PASSPHRASE is not set; the dashboard is unauthenticated")
	}

	srv, err := web.New(cfg, ix, act, reg, authn, log)
	if err != nil {
		return err
	}

	go ix.Run(ctx)
	go reg.Run(ctx)
	go authn.Run(ctx)

	httpSrv := &http.Server{
		Addr:    cfg.Addr,
		Handler: srv.Handler(),
		// A clone job runs in the background, so no handler needs a long
		// write window; the only slow one is a single-repo fetch, bounded by
		// FetchTimeout.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      cfg.FetchTimeout + 30*time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("listening",
			"addr", cfg.Addr,
			"root", cfg.Root,
			"owners", len(cfg.Owners),
			"auth", authn.Enabled(),
			"fetchInterval", cfg.FetchInterval,
		)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Warn("http shutdown", "err", err)
	}
	// In-flight clones were cancelled with the root context; wait briefly so
	// git gets a chance to exit rather than being killed mid-write.
	reg.Wait()
	return nil
}

func logLevel() slog.Level {
	switch os.Getenv("REPOMAN_LOG_LEVEL") {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
