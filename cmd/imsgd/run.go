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
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/matthewmgamble/imsgd/internal/api"
	"github.com/matthewmgamble/imsgd/internal/config"
	"github.com/matthewmgamble/imsgd/internal/contacts"
	"github.com/matthewmgamble/imsgd/internal/events"
	"github.com/matthewmgamble/imsgd/internal/imessage/db"
	"github.com/matthewmgamble/imsgd/internal/logging"
	"github.com/matthewmgamble/imsgd/internal/macos"
	"github.com/matthewmgamble/imsgd/internal/media"
	"github.com/matthewmgamble/imsgd/internal/present"
	"github.com/matthewmgamble/imsgd/internal/send"
	"github.com/matthewmgamble/imsgd/internal/store"
	"github.com/matthewmgamble/imsgd/internal/tracker"
	"github.com/matthewmgamble/imsgd/internal/watcher"
	"github.com/matthewmgamble/imsgd/internal/webhook"
)

func runServe(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file (default "+config.DefaultPath()+")")
	fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if err := cfg.ValidateForServe(); err != nil {
		return fmt.Errorf("invalid config: %w", err)
	}
	closer, err := logging.Setup(cfg.LogLevel, cfg.LogFile)
	if err != nil {
		return err
	}
	defer closer.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	osv, err := macos.OSVersion(ctx)
	if err != nil {
		return err
	}
	if ok, err := macos.CheckFullDiskAccess(cfg.MessagesDB); err != nil || !ok {
		exe, _ := os.Executable()
		return fmt.Errorf("cannot read %s: grant Full Disk Access to %s and run it via launchd (see docs/operations.md): %v",
			cfg.MessagesDB, exe, err)
	}
	d, err := db.Open(cfg.MessagesDB)
	if err != nil {
		return err
	}
	defer d.Close()
	st, err := store.Open(filepath.Join(cfg.DataDir, "imsgd.db"))
	if err != nil {
		return err
	}
	defer st.Close()

	bus := events.NewBus(events.DefaultBufferSize)
	dims := media.NewDimensionCache(50000)
	var names present.NameResolver
	var contactIndex *contacts.Index
	if cfg.Contacts.Enabled {
		contactIndex = contacts.New(contacts.DefaultDir(), cfg.Contacts.Region)
		if err := contactIndex.Load(ctx); err != nil {
			slog.Warn("contacts unavailable", "err", err)
		} else {
			st := contactIndex.Stats()
			slog.Info("contacts loaded", "databases", st.Databases, "contacts", st.Contacts)
		}
		names = contactIndex.Name
	}
	converter := media.NewConverter(filepath.Join(cfg.DataDir, "cache", "conversions"), cfg.CacheMaxMB<<20)
	presenter := &present.Presenter{DB: d, Names: names, Dims: dims}
	home, _ := os.UserHomeDir()
	sender := send.New(send.Options{
		Store: st, Bus: bus,
		Present: func(s *store.Send) any { return present.SendOf(s) },
		// Messages only sends files from under its own Attachments directory
		// and references them in place, so staged files are kept forever.
		StagingDir: filepath.Join(home, "Library", "Messages", "Attachments", "imsgd"),
	})
	if err := sender.EnsureDir(); err != nil {
		return fmt.Errorf("create attachment staging directory: %w", err)
	}
	trk := tracker.New(tracker.Options{
		DB: d, Bus: bus, Presenter: presenter,
		OnNewMessage: sender.OnNewMessage, OnMessageUpdated: sender.OnMessageUpdated,
	})
	if err := trk.Init(ctx); err != nil {
		return fmt.Errorf("initialise change tracker: %w", err)
	}
	// The dispatcher starts before the API server exists; it reads the server
	// through an atomic pointer to check that a webhook's owner key is live.
	var apiRef atomic.Pointer[api.Server]
	dispatcher := webhook.New(webhook.Options{Store: st, Bus: bus, Version: version,
		KeyExists: func(name string) bool { s := apiRef.Load(); return s != nil && s.HasKey(name) }})

	// Background workers stop when ctx is cancelled (SIGINT/SIGTERM).
	var wg sync.WaitGroup
	ticks := make(chan struct{}, 1)
	wg.Add(6)
	go func() { defer wg.Done(); converter.RunPruner(ctx) }()
	if contactIndex != nil {
		wg.Add(1)
		go func() { defer wg.Done(); contactIndex.Run(ctx, time.Minute) }()
	}
	go func() { defer wg.Done(); sender.Run(ctx) }()
	go func() { defer wg.Done(); send.KeepMessagesRunning(ctx, time.Minute) }()
	go func() {
		defer wg.Done()
		if err := watcher.Run(ctx, watcher.Options{DBPath: cfg.MessagesDB}, ticks); err != nil {
			slog.Error("file watcher unavailable; polling every 2s instead", "err", err)
			t := time.NewTicker(2 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					select {
					case ticks <- struct{}{}:
					default:
					}
				}
			}
		}
	}()
	go func() { defer wg.Done(); trk.Run(ctx, ticks) }()
	go func() { defer wg.Done(); dispatcher.Run(ctx) }()

	apiServer := api.New(api.Options{
		DB: d, APIKeys: cfg.APIKeys, Version: version, MacOSVersion: osv.String(),
		Bus: bus, Store: st, Webhooks: dispatcher,
		Sender: sender, UploadMaxBytes: cfg.UploadMaxMB << 20,
		Names: names, Dims: dims, Converter: converter, CORSOrigins: cfg.CORSOrigins,
		SendsPerMinute: cfg.SendRatePerMinute,
	})
	apiRef.Store(apiServer)
	// Reload API keys when the config file changes, so `imsgd keys
	// create/revoke` takes effect without a restart.
	wg.Add(1)
	go func() { defer wg.Done(); watchKeys(ctx, *cfgPath, apiServer) }()
	srv := &http.Server{
		Addr:              net.JoinHostPort(cfg.Listen, strconv.Itoa(cfg.Port)),
		Handler:           apiServer,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		return err
	}

	scheme := "http"
	if cfg.TLS.CertFile != "" {
		scheme = "https"
	}
	if cfg.TLS.CertFile == "" && !isLoopback(cfg.Listen) {
		slog.Warn("TLS is off: API keys and messages cross the network unencrypted; see `imsgd gen-cert`")
	}
	if warn := configPermWarning(*cfgPath); warn != "" {
		slog.Warn(warn)
	}
	slog.Info("imsgd started", "version", version, "macos", osv.String(), "addr", srv.Addr,
		"api_keys", len(cfg.APIKeys), "db", cfg.MessagesDB)
	for _, ip := range lanAddrs() {
		slog.Info("reachable at", "url", fmt.Sprintf("%s://%s", scheme, net.JoinHostPort(ip, strconv.Itoa(cfg.Port))))
	}

	errc := make(chan error, 1)
	go func() {
		if cfg.TLS.CertFile != "" {
			errc <- srv.ServeTLS(ln, cfg.TLS.CertFile, cfg.TLS.KeyFile)
		} else {
			errc <- srv.Serve(ln)
		}
	}()

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		slog.Info("shutting down")
		apiServer.Shutdown() // close WebSockets
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			return err
		}
	}
	wg.Wait()
	return nil
}

// lanAddrs lists non-loopback IPv4 addresses, for the startup log.
func lanAddrs() []string {
	var out []string
	ifaces, _ := net.Interfaces()
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				out = append(out, ipn.IP.String())
			}
		}
	}
	return out
}

// watchKeys polls the config file and applies API key changes.
func watchKeys(ctx context.Context, cfgPath string, srv *api.Server) {
	path := cfgPath
	if path == "" {
		path = config.DefaultPath()
	}
	stat := func() time.Time {
		fi, err := os.Stat(path)
		if err != nil {
			return time.Time{}
		}
		return fi.ModTime()
	}
	last := stat()
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			mod := stat()
			if mod.Equal(last) {
				continue
			}
			last = mod
			cfg, err := config.Load(cfgPath)
			if err == nil {
				err = cfg.ValidateForServe()
			}
			if err != nil {
				slog.Warn("config changed but is invalid; keeping current API keys", "err", err)
				continue
			}
			srv.SetAPIKeys(cfg.APIKeys)
			names := make([]string, len(cfg.APIKeys))
			for i, k := range cfg.APIKeys {
				names[i] = k.Name
			}
			slog.Info("API keys reloaded", "keys", names)
		}
	}
}

func isLoopback(listen string) bool {
	ip := net.ParseIP(listen)
	return listen == "localhost" || (ip != nil && ip.IsLoopback())
}

// configPermWarning reports a config file readable by other users (it holds
// API keys).
func configPermWarning(cfgPath string) string {
	if cfgPath == "" {
		cfgPath = config.DefaultPath()
	}
	fi, err := os.Stat(cfgPath)
	if err != nil || fi.Mode().Perm()&0o077 == 0 {
		return ""
	}
	return fmt.Sprintf("%s is readable by other users (mode %o) but contains API keys; run: chmod 600 %q", cfgPath, fi.Mode().Perm(), cfgPath)
}
