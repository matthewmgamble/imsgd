package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/matthewmgamble/imsgd/internal/config"
	"github.com/matthewmgamble/imsgd/internal/contacts"
	"github.com/matthewmgamble/imsgd/internal/imessage/db"
	"github.com/matthewmgamble/imsgd/internal/macos"
)

func runDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file (default "+config.DefaultPath()+")")
	automation := fs.Bool("automation", false, "also test Automation permission for Messages (launches Messages; may show a consent prompt)")
	fs.Parse(args)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	failed := false
	report := func(ok bool, name, detail string) {
		mark := "✓"
		if !ok {
			mark = "✗"
			failed = true
		}
		fmt.Printf("%s %-22s %s\n", mark, name, detail)
	}

	exe, _ := os.Executable()
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	fmt.Printf("imsgd %s (%s)\n\n", version, exe)

	// macOS ties privacy permissions to the code signature. Ad-hoc signatures
	// change on every build, silently revoking Full Disk Access.
	if out, err := exec.CommandContext(ctx, "/usr/bin/codesign", "-dr", "-", exe).CombinedOutput(); err != nil {
		report(false, "code signature", "unsigned — permissions will not persist (see docs/operations.md)")
	} else if strings.Contains(string(out), "cdhash") && !strings.Contains(string(out), "certificate") {
		report(false, "code signature", "ad-hoc — permissions are lost on every rebuild; sign with a stable identity (make install)")
	} else {
		report(true, "code signature", "stable identity")
	}

	if v, err := macos.OSVersion(ctx); err != nil {
		report(false, "macOS version", err.Error())
	} else {
		report(true, "macOS version", v.String())
	}

	fda, err := macos.CheckFullDiskAccess(cfg.MessagesDB)
	switch {
	case err != nil:
		report(false, "Messages database", err.Error())
	case !fda:
		report(false, "Full Disk Access", "denied — grant Full Disk Access to "+exe+" (or the terminal running it) in System Settings → Privacy & Security")
	default:
		report(true, "Full Disk Access", "granted")
		if d, err := db.Open(cfg.MessagesDB); err != nil {
			report(false, "Messages database", err.Error())
		} else {
			s, err := d.Stats(ctx)
			d.Close()
			if err != nil {
				report(false, "Messages database", err.Error())
			} else {
				report(true, "Messages database", fmt.Sprintf("%d messages, %d chats, %d handles (max rowid %d)", s.Messages, s.Chats, s.Handles, s.MaxRowID))
			}
		}
	}

	ci := contacts.New(contacts.DefaultDir(), cfg.Contacts.Region)
	switch {
	case !cfg.Contacts.Enabled:
		fmt.Printf("- %-22s %s\n", "Contacts", "disabled in config")
	case ci.Load(ctx) != nil || ci.Stats().Databases == 0:
		report(false, "Contacts", "AddressBook not readable (Full Disk Access?)")
	default:
		st := ci.Stats()
		report(true, "Contacts", fmt.Sprintf("%d contacts in %d databases", st.Contacts, st.Databases))
	}

	running := macos.MessagesRunning(ctx)
	report(running, "Messages.app running", map[bool]string{true: "yes", false: "no — imsgd needs Messages running to send"}[running])

	if *automation {
		err := macos.CheckMessagesAutomation(ctx)
		var ase *macos.AppleScriptError
		switch {
		case err == nil:
			report(true, "Automation (Messages)", "granted")
			out, aerr := macos.RunAppleScript(ctx, `tell application "Messages" to return (count of (every account whose enabled is true)) as text`)
			if aerr == nil && strings.TrimSpace(out) != "0" {
				report(true, "Messages accounts", strings.TrimSpace(out)+" enabled")
			} else {
				report(false, "Messages accounts", "no enabled account — sign in to iMessage in Messages")
			}
		case errors.As(err, &ase) && ase.Number == macos.ErrNumNotAuthorized:
			report(false, "Automation (Messages)", "denied — enable in System Settings → Privacy & Security → Automation")
		default:
			report(false, "Automation (Messages)", err.Error())
		}
	} else {
		fmt.Printf("- %-22s %s\n", "Automation (Messages)", "not checked (run with -automation)")
	}

	if err := cfg.ValidateForServe(); err != nil {
		report(false, "config", err.Error())
	} else {
		report(true, "config", fmt.Sprintf("%d API key(s), port %d", len(cfg.APIKeys), cfg.Port))
	}
	if warn := configPermWarning(*cfgPath); warn != "" {
		report(false, "config permissions", warn)
	}
	if cfg.TLS.CertFile != "" {
		if info, err := certInfo(cfg.TLS.CertFile, cfg.TLS.KeyFile); err != nil {
			report(false, "TLS certificate", err.Error())
		} else {
			report(true, "TLS certificate", info)
		}
	} else {
		fmt.Printf("! %-22s %s\n", "TLS", "off: keys and messages cross the LAN unencrypted (`imsgd gen-cert` to enable)")
	}
	if loaded, state, pid := agentState(); loaded {
		report(state == "running", "launchd agent", fmt.Sprintf("%s (pid %s)", state, pid))
	} else {
		fmt.Printf("- %-22s %s\n", "launchd agent", "not installed (`imsgd install-agent`)")
	}

	if failed {
		return errors.New("one or more checks failed")
	}
	return nil
}
