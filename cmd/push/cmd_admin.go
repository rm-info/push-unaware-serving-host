package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"text/tabwriter"

	"golang.org/x/term"

	"github.com/rm-info/push-unaware-serving-host/internal/admin"
	"github.com/rm-info/push-unaware-serving-host/internal/store"
	"github.com/rm-info/push-unaware-serving-host/internal/validate"
	"github.com/rm-info/push-unaware-serving-host/internal/vapid"
)

func adminCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: push admin add|passwd|rm|list ...")
	}
	sub, args := args[0], args[1:]
	fset := flag.NewFlagSet("admin "+sub, flag.ExitOnError)
	dataDir := dataFlag(fset)
	yes := false
	if sub == "rm" {
		fset.BoolVar(&yes, "yes", false, "do not ask for confirmation")
	}
	username := parseWithName(fset, args)
	if sub != "list" && username == "" {
		return fmt.Errorf("usage: push admin %s <username>", sub)
	}

	st, err := openStore(*dataDir)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()

	switch sub {
	case "add", "passwd":
		if err := validate.Username(username); err != nil {
			return err
		}
		if sub == "passwd" {
			if _, err := st.AdminByUsername(ctx, username); err != nil {
				return fmt.Errorf("admin %q: %w", username, err)
			}
		}
		hash, err := readNewPassword()
		if err != nil {
			return err
		}
		if sub == "add" {
			if err := st.CreateAdmin(ctx, username, hash); errors.Is(err, store.ErrExists) {
				return fmt.Errorf("admin %q already exists", username)
			} else if err != nil {
				return err
			}
			_ = st.Audit(ctx, cliActor, "create admin", username, "")
			fmt.Printf("Admin %q created. Sign in at /admin/.\n", username)
		} else {
			if err := st.SetAdminPassword(ctx, username, hash); err != nil {
				return err
			}
			_ = st.Audit(ctx, cliActor, "change admin password", username, "")
			fmt.Printf("Password changed; %q's sessions were closed.\n", username)
		}
		return nil

	case "rm":
		if !yes && !confirm(fmt.Sprintf("Delete admin %q? Type the username to confirm: ", username), username) {
			return errors.New("aborted")
		}
		if err := st.DeleteAdmin(ctx, username); err != nil {
			return fmt.Errorf("admin %q: %w", username, err)
		}
		_ = st.Audit(ctx, cliActor, "delete admin", username, "")
		fmt.Printf("Admin %q deleted.\n", username)
		return nil

	case "list":
		admins, err := st.ListAdmins(ctx)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "USERNAME\tCREATED")
		for _, a := range admins {
			fmt.Fprintf(tw, "%s\t%s\n", a.Username, a.CreatedAt.Format("2006-01-02"))
		}
		return tw.Flush()
	}
	return fmt.Errorf("unknown admin command %q", sub)
}

// readNewPassword reads a password twice from the terminal (no echo), or
// once from stdin when it is not a terminal (scripts, docker exec -i).
func readNewPassword() (string, error) {
	fd := int(os.Stdin.Fd())
	var p string
	if term.IsTerminal(fd) {
		fmt.Fprint(os.Stderr, "Password (12+ characters): ")
		b1, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		fmt.Fprint(os.Stderr, "Repeat: ")
		b2, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		if string(b1) != string(b2) {
			return "", errors.New("passwords do not match")
		}
		p = string(b1)
	} else {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return "", errors.New("no password on stdin")
		}
		p = strings.TrimRight(line, "\r\n")
	}
	return admin.HashPassword(p)
}

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
