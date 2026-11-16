// Command push runs PUSH (Push Unaware Serving Host), a self-hosted Web Push
// scheduler that knows nothing about what it sends, and administers it.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/rm-info/push-unaware-serving-host/internal/store"
)

var version = "dev"

const usage = `PUSH — Push Unaware Serving Host

Usage:
  push serve                          run the API, the scheduler and the web pages

  push app add <name> [flags]         register an application, print its API key
  push app list                       list applications
  push app update <name> [flags]      change origins / quotas
  push app rotate-key <name>          issue a new API key (the old one stops working)
  push app disable <name>             revoke access: key refused, pushes paused
  push app enable <name>              restore access
  push app rm <name>                  delete an application and all its data

  push request list [-all]            list API key requests (pending by default)
  push request show <id>              show a request
  push request approve <id> [flags]   approve (the requester claims the key)
  push request reject <id> [-reason]  reject

  push admin add <username>           create a web admin account (asks for a password)
  push admin passwd <username>        change a password (ends its sessions)
  push admin rm <username>            delete an account
  push admin list                     list accounts

  push vapid public-key               print the VAPID public key
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
	case "request":
		err = requestCmd(os.Args[2:])
	case "admin":
		err = adminCmd(os.Args[2:])
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

func envBool(name string, def bool) bool {
	if v := os.Getenv(name); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
		fmt.Fprintf(os.Stderr, "warning: ignoring invalid %s=%q\n", name, v)
	}
	return def
}

func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
		fmt.Fprintf(os.Stderr, "warning: ignoring invalid %s=%q\n", name, v)
	}
	return def
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

// parseWithName parses "<name> -flags" as well as "-flags <name>".
func parseWithName(fset *flag.FlagSet, args []string) string {
	var name string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		name, args = args[0], args[1:]
	}
	_ = fset.Parse(args)
	if name == "" && fset.NArg() > 0 {
		name = fset.Arg(0)
	}
	return name
}

func validateSubject(s string) error {
	low := strings.ToLower(s)
	switch {
	case s == "":
		return errors.New("a VAPID subject is required: set PUSH_VAPID_SUBJECT=mailto:you@your-real-domain (Apple rejects fake addresses)")
	case !strings.HasPrefix(low, "mailto:") && !strings.HasPrefix(low, "https://"):
		return errors.New(`VAPID subject must start with "mailto:" or "https://"`)
	case strings.Contains(low, "example.") || strings.Contains(low, "localhost") ||
		strings.Contains(low, "change-me") || strings.Contains(low, "your-domain"):
		return fmt.Errorf("VAPID subject %q looks like a placeholder: push services (Apple in particular) reject those", s)
	}
	return nil
}

func confirm(prompt, expected string) bool {
	fmt.Print(prompt)
	var answer string
	_, _ = fmt.Scanln(&answer)
	return answer == expected
}
