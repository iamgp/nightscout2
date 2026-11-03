// Command tideline runs the Tideline server and its maintenance tools.
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
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // reports need time zones even on minimal container images

	"github.com/iamgp/tideline/internal/alerts"
	"github.com/iamgp/tideline/internal/auth"
	"github.com/iamgp/tideline/internal/dexcom"
	"github.com/iamgp/tideline/internal/importer"
	"github.com/iamgp/tideline/internal/server"
	"github.com/iamgp/tideline/internal/settings"
	"github.com/iamgp/tideline/internal/store"
	"github.com/iamgp/tideline/web"
)

var version = "1.0.0"

const usage = `Tideline — a modern, Nightscout-compatible diabetes data server.

Usage:
  tideline [serve]              run the server (default)
  tideline import [flags]       copy history from an existing Nightscout site
  tideline token [flags]        create an access token
  tideline version              print the version

Configuration (environment):
  TIDELINE_API_SECRET   admin secret, at least 12 characters (API_SECRET also accepted)
  TIDELINE_DB           SQLite database path (default ./tideline.db)
  PORT                  port to listen on (default 8080)
  TIDELINE_PUBLIC_URL   public URL, used in notifications and share links
  TIDELINE_PUBLIC_READ  "true" lets anyone with the URL view data (default false)
  TIDELINE_UNITS        initial display units: mg/dl or mmol (default mg/dl)
  TIDELINE_LOG_LEVEL    debug, info, warn or error (default info)
  DEXCOM_USERNAME, DEXCOM_PASSWORD, DEXCOM_REGION (us, ous, jp)
                        pull readings from Dexcom Share
`

func main() {
	cmd := "serve"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve()
	case "import":
		err = runImport(args)
	case "token":
		err = runToken(args)
	case "version":
		fmt.Println("tideline", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func env(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

func dbPath() string {
	if p := env("TIDELINE_DB"); p != "" {
		return p
	}
	return "tideline.db"
}

func logger() *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(env("TIDELINE_LOG_LEVEL")) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

func serve() error {
	log := logger()
	secret := env("TIDELINE_API_SECRET", "API_SECRET")
	if len(secret) < 12 {
		return errors.New("TIDELINE_API_SECRET must be set to at least 12 characters")
	}
	st, err := store.Open(dbPath())
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	defaults := settings.Default()
	if u := strings.ToLower(env("TIDELINE_UNITS")); u == settings.UnitsMmol || u == "mmol/l" {
		defaults.Units = settings.UnitsMmol
	}
	if n := env("TIDELINE_NAME"); n != "" {
		defaults.Name = n
	}
	sm, err := settings.NewManager(ctx, st, defaults)
	if err != nil {
		return err
	}
	publicRead := strings.EqualFold(env("TIDELINE_PUBLIC_READ"), "true")
	authn := auth.New(secret, st, publicRead)
	publicURL := strings.TrimRight(env("TIDELINE_PUBLIC_URL"), "/")

	sender := &alerts.HTTPSender{
		Client:   &http.Client{Timeout: 15 * time.Second},
		Units:    func() string { return sm.Get().Units },
		SiteName: func() string { return sm.Get().Name },
		BaseURL:  publicURL,
	}
	srv := server.New(st, authn, sm, server.Options{
		Version:   version,
		PublicURL: publicURL,
		Log:       log,
		Static:    web.Static(),
		Sender:    sender,
	})
	go srv.Alerts().Run(ctx, 30*time.Second)

	if user := env("DEXCOM_USERNAME"); user != "" {
		base, err := dexcom.RegionURL(firstNonEmpty(env("DEXCOM_REGION"), "us"))
		if err != nil {
			return err
		}
		c := &dexcom.Client{BaseURL: base, Username: user, Password: env("DEXCOM_PASSWORD")}
		go dexcom.Poll(ctx, c, 150*time.Second, func(ctx context.Context, docs []store.Doc) error {
			_, err := st.InsertEntries(ctx, docs)
			return err
		}, log)
		log.Info("dexcom share bridge enabled", "region", firstNonEmpty(env("DEXCOM_REGION"), "us"))
	}

	addr := env("TIDELINE_ADDR")
	if addr == "" {
		addr = ":" + firstNonEmpty(env("PORT"), "8080")
	}
	hs := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	errc := make(chan error, 1)
	go func() { errc <- hs.ListenAndServe() }()
	log.Info("tideline listening", "addr", addr, "version", version, "db", dbPath(), "publicRead", publicRead)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return hs.Shutdown(sctx)
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

func runImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	src := fs.String("from", "", "Nightscout site URL, e.g. https://my-ns.example.com")
	token := fs.String("token", "", "Nightscout access token with read access")
	secret := fs.String("secret", "", "Nightscout API secret (alternative to -token)")
	days := fs.Int("days", 90, "how many days of history to copy")
	skipStatus := fs.Bool("skip-devicestatus", false, "don't copy device status (pump/loop) records")
	_ = fs.Parse(args)
	if *src == "" {
		return errors.New("-from is required")
	}
	st, err := store.Open(dbPath())
	if err != nil {
		return err
	}
	defer st.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	c, err := importer.Run(ctx, st, importer.Options{
		Source: *src, Token: *token, Secret: *secret, Days: *days, SkipStatus: *skipStatus,
		Logf: func(f string, a ...any) { fmt.Printf(f+"\n", a...) },
	})
	fmt.Printf("imported %d entries, %d treatments, %d device status records, %d profiles\n",
		c.Entries, c.Treatments, c.DeviceStatus, c.Profiles)
	return err
}

func runToken(args []string) error {
	fs := flag.NewFlagSet("token", flag.ExitOnError)
	name := fs.String("name", "", "who or what the token is for, e.g. \"xDrip phone\"")
	role := fs.String("role", "reader", "reader, uploader or admin")
	days := fs.Int("expires-days", 0, "expire after this many days (0 = never)")
	_ = fs.Parse(args)
	if *name == "" {
		return errors.New("-name is required")
	}
	if !auth.ValidRole(*role) {
		return errors.New("-role must be reader, uploader or admin")
	}
	st, err := store.Open(dbPath())
	if err != nil {
		return err
	}
	defer st.Close()
	var exp *time.Time
	if *days > 0 {
		t := time.Now().Add(time.Duration(*days) * 24 * time.Hour)
		exp = &t
	}
	plain, _, err := st.CreateToken(context.Background(), *name, *role, exp)
	if err != nil {
		return err
	}
	_ = st.Audit(context.Background(), "cli", "create token", *name+" ("+*role+")")
	fmt.Println(plain)
	return nil
}
