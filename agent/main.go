// honk-agent is the omarchy-plugin-honk backend. It talks to honk's native
// API (/api/v1) and to the plugin config file, and speaks the same command
// line and JSON-lines protocol as sbdbar-agent, so the panel QML is shared.
//
// Streams write one JSON object per line until killed, for QML's SplitParser:
//
//	watch [--interval 2s]        hello, status, groups, mode lines
//	connections [--interval 1s]  connections snapshots
//	logs [--level info]          logs batches from the native /logs SSE (reset:
//	                             true replaces the list); honk withholds the text
//	                             of unaudited messages
//
// Every stream may also write {"type":"offline",...} and then reconnects.
//
// One-shot commands print a single JSON object:
//
//	select <group> <member> | unpin <group> | urltest [group]
//	set-mode <rule|global|direct> [global-group]  (config edit, as doona does)
//	close <id> | close-all
//	get-config | save-config [--url URL] [--traffic bool]
//	set-traffic <bool> | set-record-flows <bool>
//	set-pref <groupsView|ipProvider|stunServer> <value>
//	ip [--provider ip.sb|ipwho.is|ipapi.is]   public egress IP over HTTP
//	stun [--server host:port]                 STUN test; one line per phase
//
// The API URL comes from --url, then HONK_API_URL, then the config file; the
// secret from HONK_API_SECRET, then the config file. Secrets never travel in
// argv: save-config reads HONK_API_SECRET.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

func main() {
	global := flag.NewFlagSet("honk-agent", flag.ExitOnError)
	flagURL := global.String("url", "", "honk native API URL")
	global.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: honk-agent [--url URL] <watch|connections|logs|select|unpin|urltest|set-mode|close|close-all|ip|stun|get-config|save-config|set-traffic|set-record-flows|set-pref> [args]")
	}
	_ = global.Parse(os.Args[1:])
	if global.NArg() == 0 {
		global.Usage()
		os.Exit(2)
	}
	cmd, args := global.Arg(0), global.Args()[1:]
	out := &lineWriter{w: os.Stdout}

	switch cmd {
	case "watch", "connections", "logs":
		runStream(out, *flagURL, cmd, args)
	case "select", "unpin", "urltest", "set-mode", "close", "close-all":
		u, secret := resolveEndpoint(*flagURL)
		c, err := newClient(u, secret)
		if err != nil {
			out.emit(failure(err))
			os.Exit(2)
		}
		out.emit(runAction(c, cmd, args))
	case "get-config":
		out.emit(loadConfig())
	case "save-config":
		fs := flag.NewFlagSet("save-config", flag.ExitOnError)
		u := fs.String("url", "", "native API URL")
		traffic := fs.String("traffic", "", "show traffic in the bar (true/false)")
		_ = fs.Parse(args)
		cfg := loadConfig()
		if *u != "" {
			cfg.URL = *u
		}
		if *traffic != "" {
			cfg.ShowTraffic = parseBool(*traffic)
		}
		// Secrets travel via the environment so they never show up in ps.
		if secret, ok := os.LookupEnv("HONK_API_SECRET"); ok {
			cfg.Secret = secret
		}
		writeConfig(out, cfg)
	case "set-traffic":
		cfg := loadConfig()
		cfg.ShowTraffic = len(args) == 0 || parseBool(args[0])
		writeConfig(out, cfg)
	case "set-record-flows":
		cfg := loadConfig()
		cfg.RecordFlows = len(args) == 0 || parseBool(args[0])
		writeConfig(out, cfg)
	case "set-pref":
		cfg := loadConfig()
		if len(args) != 2 {
			out.emit(map[string]any{"success": false, "error": "usage: set-pref <key> <value>"})
			os.Exit(2)
		}
		if err := setPref(&cfg, args[0], args[1]); err != nil {
			out.emit(map[string]any{"success": false, "error": err.Error()})
			os.Exit(2)
		}
		writeConfig(out, cfg)
	case "ip":
		fs := flag.NewFlagSet("ip", flag.ExitOnError)
		provider := fs.String("provider", "", "ip.sb, ipwho.is or ipapi.is (default: config)")
		_ = fs.Parse(args)
		if *provider == "" {
			*provider = loadConfig().IPProvider
		}
		out.emit(runIP(*provider))
	case "stun":
		cfg := loadConfig()
		fs := flag.NewFlagSet("stun", flag.ExitOnError)
		server := fs.String("server", cfg.STUNServer, "STUN server host:port")
		_ = fs.String("outbound", "direct", "accepted for sbdbar compatibility; routing decides the path")
		_ = fs.Parse(args)
		runSTUN(out, *server)
	default:
		global.Usage()
		os.Exit(2)
	}
}

func runStream(out *lineWriter, flagURL, cmd string, args []string) {
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	interval := fs.Duration("interval", time.Second, "push interval")
	level := fs.String("level", "info", "minimum log level")
	_ = fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	u, secret := resolveEndpoint(flagURL)
	c, err := newClient(u, secret)
	if err != nil {
		out.emit(offline(err))
		os.Exit(2)
	}
	if *interval < 500*time.Millisecond {
		*interval = 500 * time.Millisecond
	}

	switch cmd {
	case "watch":
		stream(ctx, out, c, watchSession(out, *interval, loadConfig().RecordFlows))
	case "connections":
		stream(ctx, out, c, connectionsSession(out, *interval))
	case "logs":
		lvl, ok := parseLogLevel(*level)
		if !ok {
			fmt.Fprintf(os.Stderr, "unknown log level %q\n", *level)
			os.Exit(2)
		}
		stream(ctx, out, c, logsSession(out, lvl))
	}
}

func writeConfig(out *lineWriter, cfg Config) {
	if err := saveConfig(cfg); err != nil {
		out.emit(map[string]any{"success": false, "error": err.Error()})
		os.Exit(1)
	}
	// Never echo secrets back to the QML side's stdout buffer beyond what it sent.
	shown := cfg
	shown.Secret = maskSecret(cfg.Secret)
	out.emit(map[string]any{"success": true, "config": shown})
}

func maskSecret(s string) string {
	if s == "" {
		return ""
	}
	return "********"
}

func parseBool(s string) bool {
	switch s {
	case "1", "yes", "on":
		return true
	}
	b, _ := strconv.ParseBool(s)
	return b
}
