package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/rm-info/push-unaware-serving-host/internal/admin"
	"github.com/rm-info/push-unaware-serving-host/internal/store"
	"github.com/rm-info/push-unaware-serving-host/internal/validate"
)

const cliActor = "cli"

func appCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: push app add|list|update|rotate-key|disable|enable|rm ...")
	}
	sub, args := args[0], args[1:]
	fset := flag.NewFlagSet("app "+sub, flag.ExitOnError)
	dataDir := dataFlag(fset)
	var origins, contact, description *string
	var maxSubs, maxRems *int
	if sub == "add" || sub == "update" {
		origins = fset.String("origins", "", `allowed browser origins, space or comma separated, e.g. "https://app.example.org" (empty = any)`)
		maxSubs = fset.Int("max-subscriptions", admin.DefaultMaxSubscriptions, "max subscriptions (devices) for this app")
		maxRems = fset.Int("max-reminders", admin.DefaultMaxReminders, "max reminders per subscription")
	}
	if sub == "add" {
		contact = fset.String("contact", "", "maintainer e-mail (optional)")
		description = fset.String("description", "", "description (optional)")
	}
	yes := false
	if sub == "rm" {
		fset.BoolVar(&yes, "yes", false, "do not ask for confirmation")
	}
	name := parseWithName(fset, args)
	if sub != "list" && name == "" {
		return fmt.Errorf("usage: push app %s <name>", sub)
	}

	st, err := openStore(*dataDir)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()
	audit := func(action, detail string) { _ = st.Audit(ctx, cliActor, action, name, detail) }

	switch sub {
	case "add":
		if err := validate.AppName(name); err != nil {
			return err
		}
		in := store.NewApp{Name: name, MaxSubscriptions: *maxSubs, MaxRemindersPerSub: *maxRems}
		if in.Origins, err = validate.Origins(*origins, false); err != nil {
			return err
		}
		if err := validate.Quota(in.MaxSubscriptions, "max-subscriptions"); err != nil {
			return err
		}
		if err := validate.Quota(in.MaxRemindersPerSub, "max-reminders"); err != nil {
			return err
		}
		if *contact != "" {
			if in.Contact, err = validate.Contact(*contact); err != nil {
				return err
			}
		}
		if in.Description, err = validate.Text(*description, "description", 1000, false); err != nil {
			return err
		}
		app, key, err := st.CreateApp(ctx, in)
		if errors.Is(err, store.ErrExists) {
			return fmt.Errorf("app %q already exists", name)
		}
		if err != nil {
			return err
		}
		audit("create app", "")
		fmt.Printf("App %q created.\nAPI key (shown once, give it to the app's maintainer):\n\n  %s\n\n", app.Name, key)
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
			if app.Origins, err = validate.Origins(*origins, false); err != nil {
				return err
			}
		}
		if set["max-subscriptions"] {
			app.MaxSubscriptions = *maxSubs
		}
		if set["max-reminders"] {
			app.MaxRemindersPerSub = *maxRems
		}
		if err := validate.Quota(app.MaxSubscriptions, "max-subscriptions"); err != nil {
			return err
		}
		if err := validate.Quota(app.MaxRemindersPerSub, "max-reminders"); err != nil {
			return err
		}
		if err := st.UpdateApp(ctx, app); err != nil {
			return err
		}
		audit("update app", fmt.Sprintf("origins=%s max_devices=%d max_reminders=%d", strings.Join(app.Origins, " "), app.MaxSubscriptions, app.MaxRemindersPerSub))
		fmt.Printf("App %q updated.\n", name)
		return nil

	case "list":
		apps, err := st.ListApps(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tSTATUS\tDEVICES\tREMINDERS\tMAX DEVICES\tMAX REM/DEVICE\tORIGINS\tCREATED")
		for _, a := range apps {
			o := strings.Join(a.Origins, " ")
			if o == "" {
				o = "(any)"
			}
			status := "active"
			if a.Disabled {
				status = "disabled"
			} else if !a.HasKey {
				status = "key-unclaimed"
			}
			fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%d\t%s\t%s\n", a.Name, status, a.Subscriptions, a.Reminders,
				a.MaxSubscriptions, a.MaxRemindersPerSub, o, a.CreatedAt.Format("2006-01-02"))
		}
		return tw.Flush()

	case "rotate-key":
		key, err := st.RotateAppKey(ctx, name)
		if err != nil {
			return fmt.Errorf("app %q: %w", name, err)
		}
		audit("rotate key", "")
		fmt.Printf("New API key for %q (the old one no longer works):\n\n  %s\n\n", name, key)
		return nil

	case "disable", "enable":
		if err := st.SetAppDisabled(ctx, name, sub == "disable"); err != nil {
			return fmt.Errorf("app %q: %w", name, err)
		}
		audit(sub+" app", "")
		fmt.Printf("App %q %sd.\n", name, sub)
		return nil

	case "rm":
		if !yes && !confirm(fmt.Sprintf("Delete app %q with ALL its subscriptions and reminders? Type the app name to confirm: ", name), name) {
			return errors.New("aborted")
		}
		if err := st.DeleteApp(ctx, name); err != nil {
			return fmt.Errorf("app %q: %w", name, err)
		}
		audit("delete app", "")
		fmt.Printf("App %q deleted.\n", name)
		return nil
	}
	return fmt.Errorf("unknown app command %q", sub)
}
