package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/rm-info/push-unaware-serving-host/internal/admin"
	"github.com/rm-info/push-unaware-serving-host/internal/api"
	"github.com/rm-info/push-unaware-serving-host/internal/httpx"
	"github.com/rm-info/push-unaware-serving-host/internal/keyreq"
	"github.com/rm-info/push-unaware-serving-host/internal/portal"
	"github.com/rm-info/push-unaware-serving-host/internal/pushsend"
	"github.com/rm-info/push-unaware-serving-host/internal/scheduler"
	"github.com/rm-info/push-unaware-serving-host/internal/site"
	"github.com/rm-info/push-unaware-serving-host/internal/store"
	"github.com/rm-info/push-unaware-serving-host/internal/ui"
	"github.com/rm-info/push-unaware-serving-host/internal/vapid"
	"github.com/rm-info/push-unaware-serving-host/web"
)

func serve(args []string) error {
	fset := flag.NewFlagSet("serve", flag.ExitOnError)
	dataDir := dataFlag(fset)
	listen := fset.String("listen", env("PUSH_LISTEN", "127.0.0.1:8080"), "listen address [PUSH_LISTEN]")
	subject := fset.String("vapid-subject", env("PUSH_VAPID_SUBJECT", ""), `VAPID contact, "mailto:..." or "https://..." [PUSH_VAPID_SUBJECT]`)
	grace := fset.Duration("grace", envDuration("PUSH_GRACE", time.Hour), "send occurrences at most this late, skip older ones [PUSH_GRACE]")
	ttl := fset.Duration("ttl", envDuration("PUSH_TTL", time.Hour), "how long push services keep an undelivered push [PUSH_TTL]")
	interval := fset.Duration("interval", envDuration("PUSH_INTERVAL", 15*time.Second), "scheduler scan interval [PUSH_INTERVAL]")
	anyHost := fset.Bool("allow-any-push-host", envBool("PUSH_ALLOW_ANY_PUSH_HOST", false), "accept endpoints on any https host, not only known push services [PUSH_ALLOW_ANY_PUSH_HOST]")
	keyRequests := fset.Bool("key-requests", envBool("PUSH_KEY_REQUESTS", false), "enable the public API key request form (/request-key) and API [PUSH_KEY_REQUESTS]")
	maxPending := fset.Int("max-pending-requests", envInt("PUSH_MAX_PENDING_REQUESTS", 100), "refuse new key requests beyond this many pending ones [PUSH_MAX_PENDING_REQUESTS]")
	adminUI := fset.Bool("admin-ui", envBool("PUSH_ADMIN_UI", true), "enable the web admin UI (/admin/) [PUSH_ADMIN_UI]")
	adminListen := fset.String("admin-listen", env("PUSH_ADMIN_LISTEN", ""), "serve the admin UI only on this separate address (e.g. 127.0.0.1:8081) [PUSH_ADMIN_LISTEN]")
	secureCookie := fset.Bool("admin-secure-cookie", envBool("PUSH_ADMIN_SECURE_COOKIE", true), "require HTTPS for the admin session cookie [PUSH_ADMIN_SECURE_COOKIE]")
	trusted := fset.String("trusted-proxies", env("PUSH_TRUSTED_PROXIES", "127.0.0.1/8,::1"), "reverse proxies allowed to set X-Forwarded-For [PUSH_TRUSTED_PROXIES]")
	publicURL := fset.String("public-url", env("PUSH_PUBLIC_URL", ""), "public base URL shown to integrators, e.g. https://push.example.org [PUSH_PUBLIC_URL]")
	demo := fset.Bool("demo", envBool("PUSH_DEMO", false), "serve the demo PWA under /demo/ [PUSH_DEMO]")
	debug := fset.Bool("debug", envBool("PUSH_DEBUG", false), "debug logs [PUSH_DEBUG]")
	_ = fset.Parse(args)

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	if err := validateSubject(*subject); err != nil {
		return err
	}
	if *ttl < 0 || *ttl > 28*24*time.Hour {
		return errors.New("ttl must be between 0 and 28 days")
	}
	if *interval < time.Second {
		return errors.New("interval must be at least 1s")
	}
	if *maxPending < 1 {
		return errors.New("max-pending-requests must be >= 1")
	}
	proxies, err := httpx.ParsePrefixes(*trusted)
	if err != nil {
		return fmt.Errorf("trusted-proxies: %w", err)
	}

	st, err := openStore(*dataDir)
	if err != nil {
		return err
	}
	defer st.Close()

	keys, created, err := vapid.LoadOrCreate(*dataDir)
	if err != nil {
		return fmt.Errorf("VAPID keys: %w", err)
	}
	if created {
		log.Warn("NEW VAPID KEY PAIR GENERATED — BACK IT UP NOW. If this file is lost or changed, every push subscription of every app dies.",
			"file", filepath.Join(*dataDir, vapid.FileName))
	}

	var hosts []string
	if !*anyHost {
		hosts = pushsend.DefaultPushHosts
	}

	apiSrv := &api.Server{Store: st, VAPIDPublicKey: keys.PublicKey, AllowedPushHosts: hosts, TrustedProxies: proxies, Log: log}
	if *keyRequests {
		apiSrv.KeyRequests = keyreq.New(st, log, *maxPending)
	}

	mux := http.NewServeMux()
	mux.Handle("/", apiSrv.Handler())
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", ui.Static()))
	front := (&site.Server{KeyRequests: *keyRequests, Demo: *demo, PublicURL: *publicURL, Log: log}).Handler()
	mux.Handle("GET /{$}", front)
	mux.Handle("GET /llms.txt", front)
	mux.Handle("GET /push-client.js", front)
	if *demo {
		sub, _ := fs.Sub(web.Demo, "demo")
		mux.Handle("GET /demo/", http.StripPrefix("/demo/", http.FileServerFS(sub)))
		log.Info("demo PWA enabled", "path", "/demo/")
	}
	if *keyRequests {
		p := (&portal.Server{Service: apiSrv.KeyRequests, Log: log, TrustedProxies: proxies, PublicURL: *publicURL}).Handler()
		mux.Handle("/request-key", p)
		mux.Handle("/request-key/", p)
		log.Info("public API key requests enabled", "path", "/request-key")
	}

	var servers []*http.Server
	newServer := func(addr string, h http.Handler) *http.Server {
		srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second}
		servers = append(servers, srv)
		return srv
	}
	newServer(*listen, mux)

	if *adminUI {
		adm := admin.New(admin.Config{Store: st, Log: log, TrustedProxies: proxies, SecureCookie: *secureCookie,
			RequestsEnabled: *keyRequests, PublicURL: *publicURL}).Handler()
		target := mux
		if *adminListen != "" {
			target = http.NewServeMux()
			target.Handle("GET /assets/", http.StripPrefix("/assets/", ui.Static()))
			target.Handle("GET /{$}", http.RedirectHandler("/admin/", http.StatusSeeOther))
			newServer(*adminListen, target)
		}
		target.Handle("/admin", adm)
		target.Handle("/admin/", adm)
		if n, err := st.CountAdmins(context.Background()); err == nil && n == 0 {
			log.Warn("admin UI enabled but no admin account exists: create one with \"push admin add <username>\"")
		}
		log.Info("admin UI enabled", "path", "/admin/", "listen", firstNonEmpty(*adminListen, *listen))
	}

	sched := &scheduler.Scheduler{
		Store: st,
		Sender: &pushsend.WebPush{
			PublicKey: keys.PublicKey, PrivateKey: keys.PrivateKey, Subject: *subject,
			TTL: *ttl, AllowedHosts: hosts,
			Client: &http.Client{Timeout: 30 * time.Second},
		},
		Grace:    *grace,
		Interval: *interval,
		Workers:  8,
		Log:      log,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	done := make(chan struct{})
	go func() {
		defer close(done)
		go janitor(ctx, st, log)
		sched.Run(ctx)
	}()

	errc := make(chan error, len(servers))
	for _, srv := range servers {
		go func(srv *http.Server) {
			log.Info("PUSH listening", "addr", srv.Addr, "version", version)
			errc <- srv.ListenAndServe()
		}(srv)
	}
	log.Info("scheduler started", "grace", grace.String(), "ttl", ttl.String(), "interval", interval.String())

	select {
	case err = <-errc:
	case <-ctx.Done():
		log.Info("shutting down")
	}
	stop()
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, srv := range servers {
		_ = srv.Shutdown(shutCtx)
	}
	<-done
	return err
}

// janitor purges expired sessions and old request/audit data hourly.
func janitor(ctx context.Context, st *store.Store, log *slog.Logger) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if err := st.Purge(ctx); err != nil && ctx.Err() == nil {
			log.Error("purge", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
