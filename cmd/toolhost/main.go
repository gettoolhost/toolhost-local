// toolhost — one governed front door between your agent and your MCP servers.
//
// Commands: see usage() — every command takes -c <path>; unset resolves to
// ./toolhost.json if present, else ~/.config/toolhost/toolhost.json.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/gettoolhost/toolhost-local/internal/app"
	"github.com/gettoolhost/toolhost-local/internal/config"
	"github.com/gettoolhost/toolhost-local/internal/core"
)

const cfgFlagUsage = "config path (default: ./toolhost.json, else ~/.config/toolhost/toolhost.json)"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "init":
		fs := flag.NewFlagSet("init", flag.ExitOnError)
		cfg := fs.String("c", "", cfgFlagUsage)
		_ = fs.Parse(os.Args[2:])
		err = app.Init(config.DefaultPath(*cfg), os.Stdout)

	case "discover":
		fs := flag.NewFlagSet("discover", flag.ExitOnError)
		cfg := fs.String("c", "", cfgFlagUsage)
		_ = fs.Parse(os.Args[2:])
		err = app.Discover(context.Background(), config.DefaultPath(*cfg), os.Stdout)

	case "auth":
		fs := flag.NewFlagSet("auth", flag.ExitOnError)
		cfg := fs.String("c", "", cfgFlagUsage)
		_ = fs.Parse(os.Args[2:])
		if fs.NArg() != 1 {
			fmt.Fprintln(os.Stderr, "usage: toolhost auth [-c config] <backend>")
			os.Exit(2)
		}
		err = app.Auth(context.Background(), config.DefaultPath(*cfg), fs.Arg(0), os.Stdout)

	case "approve":
		fs := flag.NewFlagSet("approve", flag.ExitOnError)
		cfg := fs.String("c", "", cfgFlagUsage)
		req := fs.Bool("requested", false, "approve every pending agent request")
		_ = fs.Parse(os.Args[2:])
		if *req {
			err = app.ApproveRequested(context.Background(), config.DefaultPath(*cfg), os.Stdout)
		} else {
			err = app.EditApprovals(context.Background(), config.DefaultPath(*cfg), fs.Args(), true, os.Stdout)
		}

	case "revoke":
		fs := flag.NewFlagSet("revoke", flag.ExitOnError)
		cfg := fs.String("c", "", cfgFlagUsage)
		_ = fs.Parse(os.Args[2:])
		err = app.EditApprovals(context.Background(), config.DefaultPath(*cfg), fs.Args(), false, os.Stdout)

	case "enable", "disable":
		fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
		cfg := fs.String("c", "", cfgFlagUsage)
		only := fs.Bool("only", false, "enable exactly these tools, hiding the rest")
		_ = fs.Parse(os.Args[2:])
		err = app.EditEnabled(config.DefaultPath(*cfg), fs.Args(), os.Args[1] == "enable", *only, os.Stdout)

	case "logout":
		fs := flag.NewFlagSet("logout", flag.ExitOnError)
		cfg := fs.String("c", "", cfgFlagUsage)
		_ = fs.Parse(os.Args[2:])
		if fs.NArg() != 1 {
			fmt.Fprintln(os.Stderr, "usage: toolhost logout [-c config] <backend>")
			os.Exit(2)
		}
		err = app.Logout(config.DefaultPath(*cfg), fs.Arg(0), os.Stdout)

	case "audit":
		fs := flag.NewFlagSet("audit", flag.ExitOnError)
		cfg := fs.String("c", "", cfgFlagUsage)
		limit := fs.Int("n", 20, "number of recent events to show (max 500)")
		kind := fs.String("kind", "", "only this event kind (tool_call, auth_failed, tool_forbidden, govern, backend_error, backend_up, schema_drift, reload)")
		_ = fs.Parse(os.Args[2:])
		err = app.Audit(config.DefaultPath(*cfg), *limit, *kind, os.Stdout)

	case "attach":
		fs := flag.NewFlagSet("attach", flag.ExitOnError)
		cfg := fs.String("c", "", cfgFlagUsage)
		stdio := fs.Bool("stdio", false, "attach as a spawned stdio server instead of the HTTP endpoint")
		printOnly := fs.Bool("print", false, "print the mcpServers JSON entry instead of writing it")
		dryRun := fs.Bool("dry-run", false, "show the planned merge without writing")
		_ = fs.Parse(os.Args[2:])
		client := ""
		switch {
		case fs.NArg() == 1:
			client = fs.Arg(0)
		case fs.NArg() > 1:
			fmt.Fprintln(os.Stderr, "usage: toolhost attach [-c config] [--stdio] [--print] [--dry-run] [<client>]")
			os.Exit(2)
		case !*printOnly:
			// No client named: survey what's installed and what's
			// already attached — no config or token needed.
			err = app.AttachDetect(os.Stdout)
		}
		if client == "" && !*printOnly {
			break
		}
		err = app.Attach(config.DefaultPath(*cfg), client, *stdio, *printOnly, *dryRun, os.Stdout)

	case "doctor":
		fs := flag.NewFlagSet("doctor", flag.ExitOnError)
		cfg := fs.String("c", "", cfgFlagUsage)
		_ = fs.Parse(os.Args[2:])
		err = app.Doctor(context.Background(), config.DefaultPath(*cfg), os.Stdout)

	case "serve":
		fs := flag.NewFlagSet("serve", flag.ExitOnError)
		cfg := fs.String("c", "", cfgFlagUsage)
		stdio := fs.Bool("stdio", false, "serve MCP over stdin/stdout instead of HTTP")
		_ = fs.Parse(os.Args[2:])
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		// stdio mode: stdout IS the protocol — status lines go to stderr.
		w := os.Stdout
		if *stdio {
			w = os.Stderr
		}
		err = app.Serve(ctx, config.DefaultPath(*cfg), w, *stdio)

	case "status":
		fs := flag.NewFlagSet("status", flag.ExitOnError)
		cfg := fs.String("c", "", cfgFlagUsage)
		_ = fs.Parse(os.Args[2:])
		err = app.Status(context.Background(), config.DefaultPath(*cfg), os.Stdout)

	case "install", "uninstall":
		fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
		cfg := fs.String("c", "", cfgFlagUsage)
		_ = fs.Parse(os.Args[2:])
		err = app.Install(config.DefaultPath(*cfg), os.Args[1] == "install", os.Stdout)

	case "version":
		fmt.Println("toolhost", core.Version)

	case "help", "-h", "--help":
		usage()

	default:
		fmt.Fprintf(os.Stderr, "toolhost: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "toolhost:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `toolhost — one governed front door between your agent and your MCP servers

  toolhost init       write a config with a fresh bearer token
  toolhost discover   list every tool your backends expose (backend__tool)
  toolhost approve    approve qualified tool names, e.g. fs__read_file
                      --requested = approve every pending agent request
  toolhost revoke     revoke qualified tool names
  toolhost enable     put approved tools on the live surface (no args = all)
                      --only <tools> = surface is exactly these
  toolhost disable    hide approved tools without un-approving them
  toolhost auth       grant upstream OAuth for a backend (browser flow)
  toolhost logout     drop a backend's stored upstream grant
  toolhost attach     register the gateway with an MCP client
                      no args = survey every known client (attached or not)
                      <claude|claude-desktop|cline|codex|cursor|devin|gemini|opencode|roo|vscode|windsurf|zed>
                      --stdio · --print · --dry-run
  toolhost audit      tail the audit log (gateway need not be running)
                      -n 20 · -kind tool_call
  toolhost doctor     check an install end to end (config, token, backends)
  toolhost status     gateway health: backends up/down, counts, requests
  toolhost serve      serve /mcp — Authorization: Bearer <token>
                      --stdio = speak MCP on stdin/stdout (attach to any client)
  toolhost install    register serve as a service (launchd / systemd --user)
  toolhost uninstall  remove the service
  toolhost version

all commands take -c <path> — default: ./toolhost.json if present,
else ~/.config/toolhost/toolhost.json (XDG_CONFIG_HOME honored)
`)
}
