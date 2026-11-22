// Command gatehouse is the Precinct model gateway.
//
//	gatehouse serve                 run the gateway
//	gatehouse token issue|revoke|list
//	gatehouse config check
//	gatehouse redact try [text]
//	gatehouse report
//	gatehouse key set|delete <name>
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/yiyiSmallti/precinct-gatehouse/internal/admin"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/api"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/config"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/keys"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/keystore"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/meter"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/oai"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/plugin"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/redact"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/store"
	"github.com/yiyiSmallti/precinct-gatehouse/internal/token"
)

var version = "0.1.0-dev"

const usage = `gatehouse — Precinct model gateway (OpenAI-compatible)

Usage:
  gatehouse serve                      run the gateway
  gatehouse apikey create --name N [--models a,b] [--groups g] [--rpm N] [--rpd N] [--concurrency N] [--ttl 0|720h] [--note s] [--json]
  gatehouse apikey list [--json]
  gatehouse apikey update <id> [--name N] [--models a,b] [--groups g] [--rpm N] [--rpd N] [--concurrency N] [--ttl 0|720h] [--note s]
  gatehouse apikey enable|disable|reset|delete <id>
  gatehouse admin set-password [--user admin]   (reads password from stdin)
  gatehouse token issue --name N [--ttl 24h] [--models p/m,...] [--groups g,...] [--max-tokens N] [--data-class internal]
  gatehouse token revoke <id|name>
  gatehouse token list
  gatehouse config check               validate the configuration
  gatehouse redact try [--target external] [--data-class internal] [text]   (reads stdin without text)
  gatehouse report [--since YYYY-MM-DD] [--format table|json|md]
  gatehouse key set <name>             store a provider key in the OS keychain (reads stdin)
  gatehouse key delete <name>
  gatehouse version

Every command takes --config (default ~/.gatehouse/config.yaml, or $GATEHOUSE_CONFIG).
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "serve":
		err = serve(args)
	case "apikey":
		err = apikeyCmd(args)
	case "admin":
		err = adminCmd(args)
	case "token":
		err = tokenCmd(args)
	case "config":
		err = configCmd(args)
	case "redact":
		err = redactCmd(args)
	case "report":
		err = report(args)
	case "key":
		err = keyCmd(args)
	case "version", "--version", "-v":
		fmt.Println("gatehouse", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		var ex exitErr
		if errors.As(err, &ex) {
			os.Exit(int(ex))
		}
		fmt.Fprintln(os.Stderr, "gatehouse:", err)
		os.Exit(1)
	}
}

type exitErr int

func (e exitErr) Error() string { return fmt.Sprintf("exit %d", int(e)) }

func newFlags(name string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	def := os.Getenv("GATEHOUSE_CONFIG")
	if def == "" {
		def = config.DefaultPath()
	}
	return fs, fs.String("config", def, "config file")
}

func load(path string) (*config.Config, error) {
	cfg, err := config.Load(path)
	if errors.Is(err, config.ErrNotFound) {
		return nil, fmt.Errorf("no config at %s (run `gatehouse serve` once to create a sample)", path)
	}
	return cfg, err
}

func serve(args []string) error {
	fs, cfgPath := newFlags("serve")
	if err := fs.Parse(args); err != nil {
		return exitErr(2)
	}
	cfg, err := config.Load(*cfgPath)
	if errors.Is(err, config.ErrNotFound) {
		if err := os.MkdirAll(filepath.Dir(*cfgPath), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(*cfgPath, []byte(config.Sample), 0o600); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, `No configuration found, so a sample was written to
  %s

Next steps:
  1. Edit providers and routing_groups in that file.
  2. Store each provider key:  gatehouse key set ds   (paste the key, then Enter)
  3. Check it:                 gatehouse config check
  4. Start again:              gatehouse serve
`, *cfgPath)
		return exitErr(2)
	}
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	srv, err := api.New(cfg, version, log)
	if err != nil {
		return err
	}
	tokPath := ""
	if cfg.Mode != "org" {
		// org mode: tokens come from the policy plugin, no local token
		_, created, err := srv.LocalToken()
		if err != nil {
			return err
		}
		tokPath = filepath.Join(cfg.DataDir, "local.token")
		if created {
			log.Info("issued the local gateway token", "file", tokPath)
		}
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		srv.Shutdown(context.Background())
		return err
	}
	log.Info("gatehouse listening", "addr", "http://"+ln.Addr().String()+"/v1", "mode", cfg.Mode, "token_file", tokPath)
	if srv.PolicyDown {
		log.Warn("policy plugin unavailable; org mode refuses every request with 503 (fail-closed)")
	}
	if len(srv.Missing) > 0 {
		log.Warn("required detectors unavailable; requests that need redaction will get 503", "detectors", srv.Missing)
	}
	var admSrv *http.Server
	if cfg.Admin.Listen != "" {
		if !admin.HasPassword(srv.DB) {
			log.Warn("admin password is not set; run `gatehouse admin set-password`")
		}
		adm := admin.New(admin.Deps{
			DB:               srv.DB,
			Keys:             srv.Keys,
			Limiter:          srv.Limiter,
			Live:             srv.Live,
			Tiered:           srv.Tiered,
			MissingDetectors: srv.Missing,
			UserKey:          srv.UserKey,
			SessionTTL:       cfg.Admin.SessionTTL.D(12 * time.Hour),
		})
		admLn, err := net.Listen("tcp", cfg.Admin.Listen)
		if err != nil {
			_ = ln.Close()
			srv.Shutdown(context.Background())
			return err
		}
		admSrv = &http.Server{Addr: cfg.Admin.Listen, Handler: adm, ReadHeaderTimeout: 10 * time.Second}
		log.Info("admin listening", "addr", "http://"+admLn.Addr().String()+"/admin/")
		defer admLn.Close()
		go func() {
			if err := admSrv.Serve(admLn); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
				log.Error("admin server failed", "err", err)
			}
		}()
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.HTTP.Serve(ln) }()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	hup := notifyReload()
	if hup != nil {
		defer signal.Stop(hup)
	}
loop:
	for {
		select {
		case <-sig:
			break loop
		case <-hup:
			res, changed, rerr := srv.Live.ReloadFromDisk("signal")
			if rerr != nil {
				log.Error("SIGHUP reload failed", "err", rerr)
			} else if changed {
				log.Info("config reloaded via SIGHUP", "version", res.Version, "restart_required", res.RestartRequired)
			} else {
				log.Info("SIGHUP received, config unchanged", "version", res.Version)
			}
		case err := <-errc:
			if err != nil && !errors.Is(err, net.ErrClosed) {
				if admSrv != nil {
					_ = admSrv.Shutdown(context.Background())
				}
				srv.Shutdown(context.Background())
				return err
			}
			break loop
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if admSrv != nil {
		_ = admSrv.Shutdown(ctx)
	}
	srv.Shutdown(ctx)
	log.Info("gatehouse stopped")
	return nil
}

func openIssuer(cfg *config.Config) (*token.Issuer, func(), error) {
	db, err := store.Open(cfg.DataDir)
	if err != nil {
		return nil, nil, err
	}
	is, err := token.NewIssuer(cfg.DataDir, db)
	if err != nil {
		db.Close()
		return nil, nil, err
	}
	return is, func() { db.Close() }, nil
}

func splitList(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

func tokenCmd(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("token: want issue, revoke or list")
	}
	sub := args[0]
	fs, cfgPath := newFlags("token " + sub)
	name := fs.String("name", "", "token name (shown in reports)")
	var ttlDefault time.Duration
	ttl := fs.Duration("ttl", 0, "lifetime, e.g. 1h (default tokens.default_ttl or 24h)")
	models := fs.String("models", "", "comma-separated <provider>/<model> the token may request")
	groups := fs.String("groups", "", "comma-separated routing groups the token may request")
	maxTokens := fs.Int64("max-tokens", 0, "total token budget (0 = unlimited)")
	dataClass := fs.String("data-class", "internal", "highest data class: public, internal, confidential")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return exitErr(2)
	}
	cfg, err := load(*cfgPath)
	if err != nil {
		return err
	}
	is, closeDB, err := openIssuer(cfg)
	if err != nil {
		return err
	}
	defer closeDB()
	switch sub {
	case "issue":
		if *name == "" {
			return fmt.Errorf("token issue: --name is required")
		}
		ttlDefault = cfg.Tokens.DefaultTTL.D(24 * time.Hour)
		if *ttl == 0 {
			*ttl = ttlDefault
		}
		tok, c, err := is.Issue(*name, *ttl, token.Scope{Groups: splitList(*groups), Models: splitList(*models), MaxTokens: *maxTokens, DataClass: *dataClass})
		if err != nil {
			return err
		}
		if *asJSON {
			return json.NewEncoder(os.Stdout).Encode(map[string]any{"token": tok, "id": c.ID, "name": c.Name, "expires_at": c.ExpiresAt, "scope": c.Scope})
		}
		fmt.Println(tok)
		fmt.Fprintf(os.Stderr, "issued %s (%s), expires %s\n", c.ID, c.Name, time.Unix(c.ExpiresAt, 0).Format(time.RFC3339))
	case "revoke":
		if fs.NArg() != 1 {
			return fmt.Errorf("token revoke: want one id or name")
		}
		n, err := is.Revoke(fs.Arg(0))
		if err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("no active token matches %q", fs.Arg(0))
		}
		fmt.Printf("revoked %d token(s)\n", n)
	case "list":
		infos, err := is.List()
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tNAME\tEXPIRES\tSTATE\tSCOPE")
		for _, i := range infos {
			exp, state := "never", "active"
			if !i.ExpiresAt.IsZero() {
				exp = i.ExpiresAt.Format(time.RFC3339)
				if time.Now().After(i.ExpiresAt) {
					state = "expired"
				}
			}
			if i.Revoked {
				state = "revoked"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", i.ID, i.Name, exp, state, i.Scope)
		}
		tw.Flush()
	default:
		return fmt.Errorf("token: unknown subcommand %q", sub)
	}
	return nil
}

func configCmd(args []string) error {
	if len(args) == 0 || args[0] != "check" {
		return fmt.Errorf("config: want check")
	}
	fs, cfgPath := newFlags("config check")
	if err := fs.Parse(args[1:]); err != nil {
		return exitErr(2)
	}
	cfg, err := load(*cfgPath)
	if err != nil {
		return err
	}
	var dbCloser func()
	resolve := keystore.Resolve
	for _, p := range cfg.Providers {
		if strings.HasPrefix(p.KeyRef, "db:") {
			db, derr := store.Open(cfg.DataDir)
			if derr != nil {
				return derr
			}
			dbCloser = func() { db.Close() }
			resolve = func(ref string) (string, error) { return api.ResolveKey(db, ref) }
			break
		}
	}
	if dbCloser != nil {
		defer dbCloser()
	}
	var problems []string
	for _, p := range cfg.Providers {
		if _, err := resolve(p.KeyRef); err != nil {
			problems = append(problems, fmt.Sprintf("providers.%s.key_ref: %v", p.Name, err))
		}
	}
	for _, pl := range cfg.Plugins {
		if pl.Enabled != nil && !*pl.Enabled {
			continue
		}
		if _, err := os.Stat(pl.Path); err != nil {
			problems = append(problems, fmt.Sprintf("plugins.%s.path: %v", pl.Name, err))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("%s:\n  - %s", cfg.Path(), strings.Join(problems, "\n  - "))
	}
	fmt.Printf("%s: ok (%d providers, %d routing groups, mode %s)\n", cfg.Path(), len(cfg.Providers), len(cfg.RoutingGroups), cfg.Mode)
	return nil
}

func redactCmd(args []string) error {
	if len(args) == 0 || args[0] != "try" {
		return fmt.Errorf("redact: want try")
	}
	fs, cfgPath := newFlags("redact try")
	target := fs.String("target", "external", "outbound target: local or external")
	dataClass := fs.String("data-class", "internal", "data class of the text")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return exitErr(2)
	}
	cfg, err := load(*cfgPath)
	if err != nil {
		return err
	}
	text := strings.Join(fs.Args(), " ")
	if text == "" {
		b, err := io.ReadAll(bufio.NewReader(os.Stdin))
		if err != nil {
			return err
		}
		text = string(b)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	host := &plugin.Host{}
	defer host.Close()
	dets, missing := host.StartDetectors(cfg, log)
	eng, err := redact.New(cfg.Redaction, dets, missing)
	if err != nil {
		return err
	}
	key := make([]byte, 32) // throwaway key: placeholders here differ from the gateway's
	if b, err := os.ReadFile(filepath.Join(cfg.DataDir, "user.key")); err == nil {
		key = b
	}
	mode := eng.Mode(*target, *dataClass)
	req := oai.Object{"messages": []any{map[string]any{"role": "user", "content": text}}}
	res, aerr := eng.Apply(context.Background(), req, mode, key, "try")
	out := map[string]any{"mode": mode, "report": res.Report}
	if aerr != nil {
		out["error"] = aerr.Error()
	} else {
		out["text"] = oai.Text(oai.Messages(req)[0])
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	fmt.Printf("mode: %s\n", mode)
	if aerr != nil {
		fmt.Printf("result: %v\n", aerr)
	} else {
		fmt.Printf("text: %s\n", out["text"])
	}
	for _, s := range res.Report.Samples {
		fmt.Printf("  %-10s len=%-3d %s\n", s.Kind, s.Length, s.Action)
	}
	for _, a := range res.Report.Alerts {
		fmt.Printf("  alert: %s\n", a)
	}
	return nil
}

func report(args []string) error {
	fs, cfgPath := newFlags("report")
	since := fs.String("since", "", "first day, YYYY-MM-DD (default: all)")
	format := fs.String("format", "table", "table, json or md")
	if err := fs.Parse(args); err != nil {
		return exitErr(2)
	}
	cfg, err := load(*cfgPath)
	if err != nil {
		return err
	}
	db, err := store.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	defer db.Close()
	rows, err := meter.New(db).Daily(*since)
	if err != nil {
		return err
	}
	switch *format {
	case "json":
		if rows == nil {
			rows = []meter.Row{}
		}
		return json.NewEncoder(os.Stdout).Encode(rows)
	case "md":
		fmt.Println("| day | token | model | requests | errors | prompt | completion | cost |")
		fmt.Println("| --- | --- | --- | ---: | ---: | ---: | ---: | ---: |")
		for _, r := range rows {
			fmt.Printf("| %s | %s | %s | %d | %d | %d | %d | %.6f |\n", r.Day, r.TokenName, r.Model, r.Requests, r.Errors, r.PromptTokens, r.CompletionTokens, r.Cost)
		}
	default:
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', tabwriter.AlignRight)
		fmt.Fprintln(tw, "DAY\tTOKEN\tMODEL\tREQUESTS\tERRORS\tPROMPT\tCOMPLETION\tCOST\t")
		for _, r := range rows {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%d\t%d\t%.6f\t\n", r.Day, r.TokenName, r.Model, r.Requests, r.Errors, r.PromptTokens, r.CompletionTokens, r.Cost)
		}
		tw.Flush()
	}
	return nil
}

func keyCmd(args []string) error {
	if len(args) != 2 || (args[0] != "set" && args[0] != "delete") {
		return fmt.Errorf("key: want `key set <name>` or `key delete <name>`")
	}
	if args[0] == "delete" {
		return keystore.Delete(args[1])
	}
	fmt.Fprintf(os.Stderr, "paste the key for %q and press Enter: ", args[1])
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return err
	}
	if err := keystore.Set(args[1], strings.TrimSpace(line)); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "stored; reference it as key_ref: keychain:%s\n", args[1])
	return nil
}

func openKeyStore(cfg *config.Config) (*keys.Store, func(), error) {
	db, err := store.Open(cfg.DataDir)
	if err != nil {
		return nil, nil, err
	}
	ks := keys.NewStore(db)
	return ks, func() {
		ks.Close()
		db.Close()
	}, nil
}

func splitFlagsAndPos(args []string, boolFlags map[string]bool) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") && a != "-" {
			flags = append(flags, a)
			name := strings.TrimLeft(a, "-")
			if eq := strings.IndexByte(name, '='); eq >= 0 {
				continue
			}
			if !boolFlags[name] && i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
			continue
		}
		pos = append(pos, a)
	}
	return append(flags, pos...)
}

func apikeyCmd(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("apikey: want create, import, list, update, enable, disable, reset or delete")
	}
	sub := args[0]
	fs, cfgPath := newFlags("apikey " + sub)
	name := fs.String("name", "", "key name")
	models := fs.String("models", "", "comma-separated allowed <provider>/<model>")
	groups := fs.String("groups", "", "comma-separated allowed routing groups")
	rpm := fs.Int("rpm", 0, "requests per minute limit (0 = unlimited)")
	rpd := fs.Int("rpd", 0, "requests per day limit (0 = unlimited)")
	conc := fs.Int("concurrency", 0, "max concurrent requests (0 = unlimited)")
	ttl := fs.Duration("ttl", 0, "lifetime, e.g. 720h (0 = never expires)")
	note := fs.String("note", "", "optional note")
	asJSON := fs.Bool("json", false, "print JSON")

	ordered := splitFlagsAndPos(args[1:], map[string]bool{"json": true})
	if err := fs.Parse(ordered); err != nil {
		return exitErr(2)
	}
	setFlags := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { setFlags[f.Name] = true })

	cfg, err := load(*cfgPath)
	if err != nil {
		return err
	}
	ks, closeKS, err := openKeyStore(cfg)
	if err != nil {
		return err
	}
	defer closeKS()

	switch sub {
	case "create":
		if strings.TrimSpace(*name) == "" {
			return fmt.Errorf("apikey create: --name is required")
		}
		var exp int64
		if *ttl > 0 {
			exp = time.Now().Add(*ttl).Unix()
		}
		plain, k, err := ks.Create(keys.CreateOpts{
			Name:        *name,
			ExpiresAt:   exp,
			Models:      splitList(*models),
			Groups:      splitList(*groups),
			RPM:         *rpm,
			RPD:         *rpd,
			Concurrency: *conc,
			Note:        *note,
		})
		if err != nil {
			return err
		}
		if *asJSON {
			return json.NewEncoder(os.Stdout).Encode(map[string]any{"key": plain, "api_key": k})
		}
		fmt.Println(plain)
		fmt.Fprintf(os.Stderr, "created %s (%s, prefix %s)\n", k.ID, k.Name, k.KeyPrefix)
	case "import":
		if strings.TrimSpace(*name) == "" {
			return fmt.Errorf("apikey import: --name is required")
		}
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		plain := strings.TrimSpace(line)
		var exp int64
		if *ttl > 0 {
			exp = time.Now().Add(*ttl).Unix()
		}
		k, err := ks.Import(plain, keys.CreateOpts{
			Name:        *name,
			ExpiresAt:   exp,
			Models:      splitList(*models),
			Groups:      splitList(*groups),
			RPM:         *rpm,
			RPD:         *rpd,
			Concurrency: *conc,
			Note:        *note,
		})
		if err != nil {
			return err
		}
		if *asJSON {
			return json.NewEncoder(os.Stdout).Encode(map[string]any{"api_key": k})
		}
		fmt.Fprintf(os.Stderr, "imported %s (%s, prefix %s)\n", k.ID, k.Name, k.KeyPrefix)
	case "list":
		list, err := ks.List()
		if err != nil {
			return err
		}
		if *asJSON {
			return json.NewEncoder(os.Stdout).Encode(list)
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tNAME\tPREFIX\tSTATE\tRPM/RPD/CONC\tMODELS\tGROUPS\tEXPIRES")
		now := time.Now()
		for _, k := range list {
			state := "enabled"
			if !k.Enabled {
				state = "disabled"
			} else if k.ExpiresAt > 0 && now.Unix() >= k.ExpiresAt {
				state = "expired"
			}
			exp := "never"
			if k.ExpiresAt > 0 {
				exp = time.Unix(k.ExpiresAt, 0).Format(time.RFC3339)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d/%d/%d\t%s\t%s\t%s\n",
				k.ID, k.Name, k.KeyPrefix, state, k.RPM, k.RPD, k.Concurrency,
				strings.Join(k.Models, ","), strings.Join(k.Groups, ","), exp)
		}
		tw.Flush()
	case "update":
		if fs.NArg() != 1 {
			return fmt.Errorf("apikey update: want <id>")
		}
		var patch keys.Patch
		if setFlags["name"] {
			patch.Name = name
		}
		if setFlags["models"] {
			m := splitList(*models)
			patch.Models = &m
		}
		if setFlags["groups"] {
			g := splitList(*groups)
			patch.Groups = &g
		}
		if setFlags["rpm"] {
			patch.RPM = rpm
		}
		if setFlags["rpd"] {
			patch.RPD = rpd
		}
		if setFlags["concurrency"] {
			patch.Concurrency = conc
		}
		if setFlags["note"] {
			patch.Note = note
		}
		if setFlags["ttl"] {
			var exp int64
			if *ttl > 0 {
				exp = time.Now().Add(*ttl).Unix()
			}
			patch.ExpiresAt = &exp
		}
		k, err := ks.Update(fs.Arg(0), patch)
		if err != nil {
			return err
		}
		if *asJSON {
			return json.NewEncoder(os.Stdout).Encode(k)
		}
		fmt.Printf("updated %s (%s)\n", k.ID, k.Name)
	case "enable", "disable":
		if fs.NArg() != 1 {
			return fmt.Errorf("apikey %s: want <id>", sub)
		}
		if err := ks.SetEnabled(fs.Arg(0), sub == "enable"); err != nil {
			return err
		}
		fmt.Printf("%sd %s\n", sub, fs.Arg(0))
	case "reset":
		if fs.NArg() != 1 {
			return fmt.Errorf("apikey reset: want <id>")
		}
		plain, k, err := ks.Reset(fs.Arg(0))
		if err != nil {
			return err
		}
		if *asJSON {
			return json.NewEncoder(os.Stdout).Encode(map[string]any{"key": plain, "api_key": k})
		}
		fmt.Println(plain)
		fmt.Fprintf(os.Stderr, "reset %s (%s, prefix %s)\n", k.ID, k.Name, k.KeyPrefix)
	case "delete":
		if fs.NArg() != 1 {
			return fmt.Errorf("apikey delete: want <id>")
		}
		if err := ks.Delete(fs.Arg(0)); err != nil {
			return err
		}
		fmt.Printf("deleted %s\n", fs.Arg(0))
	default:
		return fmt.Errorf("apikey: unknown subcommand %q", sub)
	}
	return nil
}

func adminCmd(args []string) error {
	if len(args) == 0 || args[0] != "set-password" {
		return fmt.Errorf("admin: want set-password [--user admin]")
	}
	fs, cfgPath := newFlags("admin set-password")
	user := fs.String("user", "admin", "admin username")
	ordered := splitFlagsAndPos(args[1:], nil)
	if err := fs.Parse(ordered); err != nil {
		return exitErr(2)
	}
	cfg, err := load(*cfgPath)
	if err != nil {
		return err
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	pass := strings.TrimRight(line, "\r\n")
	if pass == "" {
		return fmt.Errorf("admin set-password: empty password on stdin")
	}
	db, err := store.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := admin.SetPassword(db, *user, pass); err != nil {
		return err
	}
	fmt.Printf("admin password updated for %q\n", strings.TrimSpace(*user))
	return nil
}
