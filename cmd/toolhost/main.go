// toolhost — one governed front door between your agent and your MCP servers.
//
// Commands:
//
//	toolhost init       [-c toolhost.json]   write a fresh config + token
//	toolhost discover   [-c toolhost.json]   list every tool the backends expose
//	toolhost approve    [-c …] NAME…         approve qualified tool names
//	toolhost revoke     [-c …] NAME…         revoke qualified tool names
//	toolhost serve      [-c toolhost.json]   serve /mcp until interrupted
//	toolhost version
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"toolhost/internal/app"
	"toolhost/internal/core"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "init":
		fs := flag.NewFlagSet("init", flag.ExitOnError)
		cfg := fs.String("c", "toolhost.json", "config path")
		_ = fs.Parse(os.Args[2:])
		err = app.Init(*cfg, os.Stdout)

	case "discover":
		fs := flag.NewFlagSet("discover", flag.ExitOnError)
		cfg := fs.String("c", "toolhost.json", "config path")
		_ = fs.Parse(os.Args[2:])
		err = app.Discover(context.Background(), *cfg, os.Stdout)

	case "approve", "revoke":
		fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
		cfg := fs.String("c", "toolhost.json", "config path")
		_ = fs.Parse(os.Args[2:])
		err = app.EditApprovals(*cfg, fs.Args(), os.Args[1] == "approve", os.Stdout)

	case "serve":
		fs := flag.NewFlagSet("serve", flag.ExitOnError)
		cfg := fs.String("c", "toolhost.json", "config path")
		_ = fs.Parse(os.Args[2:])
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		err = app.Serve(ctx, *cfg, os.Stdout)

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

  toolhost init       write toolhost.json with a fresh bearer token
  toolhost discover   list every tool your backends expose (backend__tool)
  toolhost approve    approve qualified tool names, e.g. fs__read_file
  toolhost revoke     revoke qualified tool names
  toolhost serve      serve /mcp — Authorization: Bearer <token>
  toolhost version

all commands take -c <path> (default toolhost.json)
`)
}
