// toolhost — one governed front door between your agent and your MCP servers.
//
// Commands: see usage() — every command takes -c <path>; unset resolves to
// the folder's private env, then ./.toolhost/toolhost.json, then the global
// ~/.toolhost/toolhost.json.
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

const cfgFlagUsage = "config path (default: private env, else ./.toolhost/toolhost.json, else ~/.toolhost/toolhost.json)"

func main() {
	if len(os.Args) < 2 {
		// Bare toolhost is adaptive: no env → the golden path; an env → status.
		if _, statErr := os.Stat(config.DefaultPath("")); statErr == nil {
			_ = app.Status(context.Background(), config.DefaultPath(""), false, os.Stdout)
		} else {
			fmt.Fprint(os.Stdout, goldenPath)
			os.Exit(2)
		}
		return
	}

	var err error
	switch os.Args[1] {
	case "init":
		fs := flag.NewFlagSet("init", flag.ExitOnError)
		cfg := fs.String("c", "", cfgFlagUsage)
		project := fs.Bool("project", false, "create this folder's private env under ~/.toolhost/projects/")
		shared := fs.Bool("shared", false, "create a committable ./.toolhost/toolhost.json (env: token ref, no literal secret)")
		_ = fs.Parse(os.Args[2:])
		switch {
		case *project && *shared:
			err = fmt.Errorf("--project and --shared are mutually exclusive")
		case *project:
			err = app.InitProject(os.Stdout)
		case *shared:
			err = app.InitShared(os.Stdout)
		default:
			err = app.Init(config.DefaultPath(*cfg), os.Stdout)
		}

	case "mcp":
		err = app.MCP(context.Background(), os.Args[2:], os.Stdout)

	case "discover":
		fs := flag.NewFlagSet("discover", flag.ExitOnError)
		cfg := fs.String("c", "", cfgFlagUsage)
		asJSON := fs.Bool("json", false, "machine-readable catalog")
		_ = fs.Parse(os.Args[2:])
		err = app.Discover(context.Background(), config.DefaultPath(*cfg), *asJSON, os.Stdout)

	case "approve":
		fs := flag.NewFlagSet("approve", flag.ExitOnError)
		cfg := fs.String("c", "", cfgFlagUsage)
		req := fs.Bool("requested", false, "approve every pending agent tool request")
		dry := fs.Bool("dry-run", false, "show the pins and diff without writing")
		_ = fs.Parse(os.Args[2:])
		if *req {
			err = app.ApproveRequested(context.Background(), config.DefaultPath(*cfg), os.Stdout)
		} else {
			err = app.EditApprovals(context.Background(), config.DefaultPath(*cfg), fs.Args(), true, *dry, os.Stdout)
		}

	case "revoke":
		fs := flag.NewFlagSet("revoke", flag.ExitOnError)
		cfg := fs.String("c", "", cfgFlagUsage)
		dry := fs.Bool("dry-run", false, "show the diff without writing")
		_ = fs.Parse(os.Args[2:])
		err = app.EditApprovals(context.Background(), config.DefaultPath(*cfg), fs.Args(), false, *dry, os.Stdout)

	case "requests":
		err = app.Requests(context.Background(), os.Args[2:], os.Stdout)

	case "enable", "disable":
		fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
		cfg := fs.String("c", "", cfgFlagUsage)
		only := fs.Bool("only", false, "enable exactly these tools, hiding the rest")
		_ = fs.Parse(os.Args[2:])
		err = app.EditEnabled(config.DefaultPath(*cfg), fs.Args(), os.Args[1] == "enable", *only, os.Stdout)

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
		project := fs.Bool("project", false, "write the client project-scope file in this folder (local-gateway, stdio spawn)")
		printOnly := fs.Bool("print", false, "print the mcpServers JSON entry instead of writing it")
		dryRun := fs.Bool("dry-run", false, "show the planned merge without writing")
		_ = fs.Parse(os.Args[2:])
		client := ""
		switch {
		case fs.NArg() == 1:
			client = fs.Arg(0)
		case fs.NArg() > 1:
			fmt.Fprintln(os.Stderr, "usage: toolhost attach [-c config] [--project] [--stdio] [--print] [--dry-run] [<client>]")
			os.Exit(2)
		case !*printOnly:
			// No client named: survey what's installed and what's
			// already attached — no config or token needed.
			err = app.AttachDetect(os.Stdout)
		}
		if client == "" && !*printOnly {
			break
		}
		err = app.Attach(config.DefaultPath(*cfg), client, *stdio, *project, *printOnly, *dryRun, os.Stdout)

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
		asJSON := fs.Bool("json", false, "machine-readable report")
		_ = fs.Parse(os.Args[2:])
		err = app.Status(context.Background(), config.DefaultPath(*cfg), *asJSON, os.Stdout)

	case "install", "uninstall":
		fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
		cfg := fs.String("c", "", cfgFlagUsage)
		_ = fs.Parse(os.Args[2:])
		err = app.Install(config.DefaultPath(*cfg), os.Args[1] == "install", os.Stdout)

	case "version":
		fmt.Println("toolhost", core.Version)

	case "completion":
		err = app.Completion(os.Args[2:], os.Stdout)

	case "help", "-h", "--help":
		app.Help(os.Args[2:], os.Stderr)

	default:
		fmt.Fprintf(os.Stderr, "toolhost: unknown command %q", os.Args[1])
		if near := app.Suggest(os.Args[1]); near != "" {
			fmt.Fprintf(os.Stderr, " — did you mean %q?", near)
		}
		fmt.Fprintln(os.Stderr)
		app.Help(nil, os.Stderr)
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "toolhost:", err)
		os.Exit(1)
	}
}

const goldenPath = `toolhost — one governed front door between your agent and your MCP servers

  toolhost init                  write the config + a fresh bearer token
  toolhost mcp add fs -- npx -y @modelcontextprotocol/server-filesystem /tmp
  toolhost discover              every tool the backend exposes
  toolhost approve fs__read_file approve what the agent may use
  toolhost serve                 run the governed /mcp
  toolhost attach claude         give the client one server: local-gateway

  toolhost help                  every verb, grouped by job
`
