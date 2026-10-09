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

	case "approve", "revoke":
		fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
		cfg := fs.String("c", "", cfgFlagUsage)
		_ = fs.Parse(os.Args[2:])
		err = app.EditApprovals(context.Background(), config.DefaultPath(*cfg), fs.Args(), os.Args[1] == "approve", os.Stdout)

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

	case "attach":
		fs := flag.NewFlagSet("attach", flag.ExitOnError)
		cfg := fs.String("c", "", cfgFlagUsage)
		stdio := fs.Bool("stdio", false, "attach as a spawned stdio server instead of the HTTP endpoint")
		printOnly := fs.Bool("print", false, "print the mcpServers JSON entry instead of writing it")
		_ = fs.Parse(os.Args[2:])
		client := ""
		if fs.NArg() == 1 {
			client = fs.Arg(0)
		} else if fs.NArg() > 1 || !*printOnly {
			fmt.Fprintln(os.Stderr, "usage: toolhost attach [-c config] [--stdio] [--print] <claude|devin|cursor|windsurf>")
			os.Exit(2)
		}
		err = app.Attach(config.DefaultPath(*cfg), client, *stdio, *printOnly, os.Stdout)

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
  toolhost revoke     revoke qualified tool names
  toolhost enable     put approved tools on the live surface (no args = all)
                      --only <tools> = surface is exactly these
  toolhost disable    hide approved tools without un-approving them
  toolhost auth       grant upstream OAuth for a backend (browser flow)
  toolhost logout     drop a backend's stored upstream grant
  toolhost attach     register the gateway with an MCP client
                      <claude|devin|cursor|windsurf> · --stdio · --print
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
