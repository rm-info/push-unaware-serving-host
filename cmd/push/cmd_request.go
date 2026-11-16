package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/rm-info/push-unaware-serving-host/internal/admin"
	"github.com/rm-info/push-unaware-serving-host/internal/store"
	"github.com/rm-info/push-unaware-serving-host/internal/validate"
)

func requestCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: push request list|show|approve|reject ...")
	}
	sub, args := args[0], args[1:]
	fset := flag.NewFlagSet("request "+sub, flag.ExitOnError)
	dataDir := dataFlag(fset)
	all := false
	var name, origins, reason *string
	var maxSubs, maxRems *int
	switch sub {
	case "list":
		fset.BoolVar(&all, "all", false, "include decided requests")
	case "approve":
		name = fset.String("name", "", "application name (default: the requested one)")
		origins = fset.String("origins", "", "allowed origins (default: the requested ones)")
		maxSubs = fset.Int("max-subscriptions", admin.DefaultMaxSubscriptions, "max subscriptions (devices)")
		maxRems = fset.Int("max-reminders", admin.DefaultMaxReminders, "max reminders per subscription")
	case "reject":
		reason = fset.String("reason", "", "reason shown to the requester")
	}
	idStr := parseWithName(fset, args)

	st, err := openStore(*dataDir)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()

	if sub == "list" {
		status := store.RequestPending
		if all {
			status = ""
		}
		rs, err := st.ListKeyRequests(ctx, status)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tSTATUS\tNAME\tCONTACT\tORIGINS\tSUBMITTED")
		for _, r := range rs {
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", r.ID, r.Status, r.Name, r.Contact, strings.Join(r.Origins, " "), r.CreatedAt.Format("2006-01-02 15:04"))
		}
		return tw.Flush()
	}

	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return fmt.Errorf("usage: push request %s <id>", sub)
	}
	kr, err := st.KeyRequestByID(ctx, id)
	if err != nil {
		return fmt.Errorf("request %d: %w", id, err)
	}

	switch sub {
	case "show":
		fmt.Printf("Request #%d (%s)\n  name:        %s\n  contact:     %s\n  origins:     %s\n  submitted:   %s\n  description: %s\n",
			kr.ID, kr.Status, kr.Name, kr.Contact, strings.Join(kr.Origins, " "), kr.CreatedAt.Format("2006-01-02 15:04 MST"),
			strings.ReplaceAll(kr.Description, "\n", "\n               "))
		if kr.Status != store.RequestPending {
			fmt.Printf("  decided:     %s by %s\n", kr.DecidedAt.Format("2006-01-02 15:04 MST"), kr.DecidedBy)
		}
		if kr.Reason != "" {
			fmt.Printf("  reason:      %s\n", kr.Reason)
		}
		return nil

	case "approve":
		in := store.NewApp{Name: kr.Name, Origins: kr.Origins, MaxSubscriptions: *maxSubs, MaxRemindersPerSub: *maxRems,
			Contact: kr.Contact, Description: kr.Description}
		if *name != "" {
			in.Name = *name
		}
		if err := validate.AppName(in.Name); err != nil {
			return err
		}
		if *origins != "" {
			if in.Origins, err = validate.Origins(*origins, false); err != nil {
				return err
			}
		}
		if err := validate.Quota(in.MaxSubscriptions, "max-subscriptions"); err != nil {
			return err
		}
		if err := validate.Quota(in.MaxRemindersPerSub, "max-reminders"); err != nil {
			return err
		}
		if _, err := st.ApproveKeyRequest(ctx, id, in, cliActor); err != nil {
			if errors.Is(err, store.ErrExists) {
				return fmt.Errorf("an app named %q already exists: use -name", in.Name)
			}
			return err
		}
		_ = st.Audit(ctx, cliActor, "approve request", in.Name, fmt.Sprintf("request #%d", id))
		fmt.Printf("Request #%d approved: app %q created. The requester retrieves the key with their status token.\n", id, in.Name)
		return nil

	case "reject":
		r, err := validate.Text(*reason, "reason", 500, false)
		if err != nil {
			return err
		}
		if err := st.RejectKeyRequest(ctx, id, r, cliActor); err != nil {
			return err
		}
		_ = st.Audit(ctx, cliActor, "reject request", kr.Name, fmt.Sprintf("request #%d", id))
		fmt.Printf("Request #%d rejected.\n", id)
		return nil
	}
	return fmt.Errorf("unknown request command %q", sub)
}
