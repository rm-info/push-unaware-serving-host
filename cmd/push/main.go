// Command push runs PUSH (Push Unaware Serving Host), a self-hosted Web Push
// scheduler that knows nothing about what it sends, and administers it.
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
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/rm-info/push-unaware-serving-host/internal/api"
	"github.com/rm-info/push-unaware-serving-host/internal/pushsend"
	"github.com/rm-info/push-unaware-serving-host/internal/scheduler"
	"github.com/rm-info/push-unaware-serving-host/internal/store"
	"github.com/rm-info/push-unaware-serving-host/internal/vapid"
	"github.com/rm-info/push-unaware-serving-host/web"
)

var version = "dev"

const usage = `PUSH — Push Unaware Serving Host

Usage:
  push serve                       run the API and the scheduler
  push app add <name> [flags]      register an application, print its API key
  push app list                    list applications
  push app update <name> [flags]   change origins / quotas
  push app rotate-key <name>       issue a new API key (the old one stops working)
  push app rm <name>               delete an application and all its data
  push vapid public-key            print the VAPID public key
  push version

Every command accepts -data <dir> (env PUSH_DATA_DIR, default ./data).
Run "push <command> -h" for the flags of a command.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "app":
		err = appCmd(os.Args[2:])
	case "vapid":
		err = vapidCmd(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println("push", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// --- configuration helpers --------------------------------------------------

func env(name, def string) string {
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return v
	}
	return def
}

func envDuration(name string, def time.Duration) time.Duration {
	if v := os.Getenv(name); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		fmt.Fprintf(os.Stderr, "warning: ignoring invalid %s=%q\n", name, v)
	}
	return def
}

func envBool(name string) bool {
	b, _ := strconv.ParseBool(os.Getenv(name))
	return b
}

func dataFlag(fs *flag.FlagSet) *string {
	return fs.String("data", env("PUSH_DATA_DIR", "./data"), "data directory (database + VAPID keys) [PUSH_DATA_DIR]")
}

func openStore(dir string) (*store.Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return store.Open(filepath.Join(dir, "push.db"))
}

func validateSubject(s string) error {
	low := strings.ToLower(s)
	switch {
	case s == "":
		return errors.New("a VAPID subject is required: set PUSH_VAPID_SUBJECT=mailto:you@your-domain (a real, reachable address: Apple rejects fake ones)")
	case !strings.HasPrefix(low, "mailto:") && !strings.HasPrefix(low, "https://"):
		return errors.New(`VAPID subject must start with "mailto:" or "https://"`)
	case strings.Contains(low, "example.") || strings.Contains(low, "localhost") ||
		strings.Contains(low, "change-me") || strings.Contains(low, "your-domain"):
		return fmt.Errorf("VAPID subject %q looks like a placeholder: push services (Apple in particular) reject those", s)
	}
	return nil
}

// --- serve ------------------------------------------------------------------

func serve(args []string) error {
	fset := flag.NewFlagSet("serve", flag.ExitOnError)
	dataDir := dataFlag(fset)
	listen := fset.String("listen", env("PUSH_LISTEN", "127.0.0.1:8080"), "listen address [PUSH_LISTEN]")
	subject := fset.String("vapid-subject", env("PUSH_VAPID_SUBJECT", ""), `VAPID contact, "mailto:..." or "https://..." [PUSH_VAPID_SUBJECT]`)
	grace := fset.Duration("grace", envDuration("PUSH_GRACE", time.Hour), "send occurrences at most this late, skip older ones [PUSH_GRACE]")
	ttl := fset.Duration("ttl", envDuration("PUSH_TTL", time.Hour), "how long push services keep an undelivered push [PUSH_TTL]")
	interval := fset.Duration("interval", envDuration("PUSH_INTERVAL", 15*time.Second), "scheduler scan interval [PUSH_INTERVAL]")
	anyHost := fset.Bool("allow-any-push-host", envBool("PUSH_ALLOW_ANY_PUSH_HOST"), "accept endpoints on any https host, not only known push services [PUSH_ALLOW_ANY_PUSH_HOST]")
	demo := fset.Bool("demo", envBool("PUSH_DEMO"), "serve the demo PWA under /demo/ [PUSH_DEMO]")
	debug := fset.Bool("debug", envBool("PUSH_DEBUG"), "debug logs [PUSH_DEBUG]")
	_ = fset.Parse(args)

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	if err := validateSubject(*subject); err != nil {
		return err
	}
	if *ttl < 0 || *ttl > 28*24*time.Hour {
		return errors.New("ttl must be between 0 and 28 days")
	}
	if *interval < time.Second {
		return errors.New("interval must be at least 1s")
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
	keyPath := filepath.Join(*dataDir, vapid.FileName)
	if created {
		log.Warn("NEW VAPID KEY PAIR GENERATED — BACK IT UP NOW. If this file is lost or changed, every push subscription of every app dies.", "file", keyPath)
	}

	var hosts []string
	if !*anyHost {
		hosts = pushsend.DefaultPushHosts
	}

	apiSrv := &api.Server{Store: st, VAPIDPublicKey: keys.PublicKey, AllowedPushHosts: hosts, Log: log}
	mux := http.NewServeMux()
	mux.Handle("/", apiSrv.Handler())
	if *demo {
		sub, _ := fs.Sub(web.Demo, "demo")
		mux.Handle("GET /demo/", http.StripPrefix("/demo/", http.FileServerFS(sub)))
		log.Info("demo PWA enabled", "path", "/demo/")
	}

	httpSrv := &http.Server{
		Addr:              *listen,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
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

	schedDone := make(chan struct{})
	go func() {
		defer close(schedDone)
		sched.Run(ctx)
	}()

	errc := make(chan error, 1)
	go func() {
		log.Info("PUSH listening", "addr", *listen, "version", version, "grace", grace.String(), "ttl", ttl.String())
		errc <- httpSrv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		stop()
		<-schedDone
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutCtx)
	<-schedDone
	return nil
}

// --- app --------------------------------------------------------------------

const (
	defaultMaxSubs = 10000
	defaultMaxRems = 100
)

func appCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: push app add|list|update|rotate-key|rm ...")
	}
	sub, args := args[0], args[1:]
	fset := flag.NewFlagSet("app "+sub, flag.ExitOnError)
	dataDir := dataFlag(fset)
	var origins *string
	var maxSubs, maxRems *int
	if sub == "add" || sub == "update" {
		origins = fset.String("origins", "", `space-separated allowed browser origins, e.g. "https://app.example.org" (empty = any)`)
		maxSubs = fset.Int("max-subscriptions", defaultMaxSubs, "max subscriptions (devices) for this app")
		maxRems = fset.Int("max-reminders", defaultMaxRems, "max reminders per subscription")
	}
	yes := false
	if sub == "rm" {
		fset.BoolVar(&yes, "yes", false, "do not ask for confirmation")
	}
	// Accept "<name> -flags" as well as "-flags <name>".
	var name string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	_ = fset.Parse(args)
	if name == "" && fset.NArg() > 0 {
		name = fset.Arg(0)
	}
	if sub != "list" && name == "" {
		return fmt.Errorf("usage: push app %s <name>", sub)
	}

	st, err := openStore(*dataDir)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()

	switch sub {
	case "add":
		if err := checkOrigins(*origins); err != nil {
			return err
		}
		if *maxSubs < 1 || *maxRems < 1 {
			return errors.New("quotas must be >= 1")
		}
		app, key, err := st.CreateApp(ctx, name, strings.Fields(*origins), *maxSubs, *maxRems)
		if errors.Is(err, store.ErrExists) {
			return fmt.Errorf("app %q already exists", name)
		}
		if err != nil {
			return err
		}
		fmt.Printf("App %q created.\nAPI key (shown once, store it in your PWA config):\n\n  %s\n\n", app.Name, key)
		if len(app.Origins) == 0 {
			fmt.Println("Warning: no -origins set, any website may use this key from a browser.")
		}
		return nil

	case "update":
		app, err := st.AppByName(ctx, name)
		if err != nil {
			return fmt.Errorf("app %q: %w", name, err)
		}
		set := map[string]bool{}
		fset.Visit(func(f *flag.Flag) { set[f.Name] = true })
		if set["origins"] {
			if err := checkOrigins(*origins); err != nil {
				return err
			}
			app.Origins = strings.Fields(*origins)
		}
		if set["max-subscriptions"] {
			app.MaxSubscriptions = *maxSubs
		}
		if set["max-reminders"] {
			app.MaxRemindersPerSub = *maxRems
		}
		if app.MaxSubscriptions < 1 || app.MaxRemindersPerSub < 1 {
			return errors.New("quotas must be >= 1")
		}
		if err := st.UpdateApp(ctx, app); err != nil {
			return err
		}
		fmt.Printf("App %q updated.\n", name)
		return nil

	case "list":
		apps, err := st.ListApps(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tSUBSCRIPTIONS\tREMINDERS\tMAX SUBS\tMAX REM/SUB\tORIGINS\tCREATED")
		for _, a := range apps {
			o := strings.Join(a.Origins, " ")
			if o == "" {
				o = "(any)"
			}
			fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%s\t%s\n", a.Name, a.Subscriptions, a.Reminders,
				a.MaxSubscriptions, a.MaxRemindersPerSub, o, a.CreatedAt.Format("2006-01-02"))
		}
		return tw.Flush()

	case "rotate-key":
		key, err := st.RotateAppKey(ctx, name)
		if err != nil {
			return fmt.Errorf("app %q: %w", name, err)
		}
		fmt.Printf("New API key for %q (the old one no longer works):\n\n  %s\n\n", name, key)
		return nil

	case "rm":
		if !yes {
			fmt.Printf("Delete app %q with ALL its subscriptions and reminders? Type the app name to confirm: ", name)
			var answer string
			_, _ = fmt.Scanln(&answer)
			if answer != name {
				return errors.New("aborted")
			}
		}
		if err := st.DeleteApp(ctx, name); err != nil {
			return fmt.Errorf("app %q: %w", name, err)
		}
		fmt.Printf("App %q deleted.\n", name)
		return nil
	}
	return fmt.Errorf("unknown app command %q", sub)
}

func checkOrigins(s string) error {
	for _, o := range strings.Fields(s) {
		if o == "*" {
			continue
		}
		if !(strings.HasPrefix(o, "https://") || strings.HasPrefix(o, "http://")) || strings.Count(o, "/") != 2 {
			return fmt.Errorf("invalid origin %q (want scheme://host[:port], no path)", o)
		}
	}
	return nil
}

// --- vapid ------------------------------------------------------------------

func vapidCmd(args []string) error {
	if len(args) == 0 || args[0] != "public-key" {
		return errors.New("usage: push vapid public-key")
	}
	fset := flag.NewFlagSet("vapid", flag.ExitOnError)
	dataDir := dataFlag(fset)
	_ = fset.Parse(args[1:])
	k, err := vapid.Load(*dataDir)
	if errors.Is(err, fs.ErrNotExist) {
		return errors.New("no VAPID key yet: it is generated on the first \"push serve\"")
	}
	if err != nil {
		return err
	}
	fmt.Println(k.PublicKey)
	return nil
}
