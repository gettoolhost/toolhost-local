// requestscli.go — `toolhost requests`, the human's petition-review
// surface. gateway__request files petitions into the env's queue file;
// this command is the other side of the conversation.
package app

import (
	"context"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/gettoolhost/toolhost-local/internal/config"
)

// Requests dispatches the petition queue: list pending (or all with
// --all), grant one with --accept <name>, close one with --deny <name>.
// Tool petitions approve into policy; server petitions materialize into
// config — the agent's ask becomes a backend entry the human then
// governs like any other.
func Requests(ctx context.Context, argv []string, w io.Writer) error {
	var cfg, accept, deny, reason string
	var all, asJSON bool
	for i := 0; i < len(argv); i++ {
		switch argv[i] {
		case "-c":
			i++
			if i >= len(argv) {
				return fmt.Errorf("-c needs a value")
			}
			cfg = argv[i]
		case "--accept":
			i++
			if i >= len(argv) {
				return fmt.Errorf("--accept needs a name")
			}
			accept = argv[i]
		case "--deny":
			i++
			if i >= len(argv) {
				return fmt.Errorf("--deny needs a name")
			}
			deny = argv[i]
		case "--reason":
			i++
			if i >= len(argv) {
				return fmt.Errorf("--reason needs text")
			}
			reason = argv[i]
		case "--all":
			all = true
		case "--json":
			asJSON = true
		default:
			return fmt.Errorf("unknown argument %q — usage: toolhost requests [-c config] [--all] [--accept <name>] [--deny <name> --reason …]", argv[i])
		}
	}
	path := config.DefaultPath(cfg)

	switch {
	case accept != "":
		return acceptRequest(ctx, path, accept, w)
	case deny != "":
		if err := config.DenyRequest(path, deny, reason); err != nil {
			return err
		}
		fmt.Fprintf(w, "denied %s — the agent sees the verdict\n", deny)
		return nil
	}
	return listRequests(path, all, asJSON, w)
}

func listRequests(path string, all, asJSON bool, w io.Writer) error {
	reqs, err := config.LoadRequests(path)
	if err != nil {
		return err
	}
	shown := reqs
	if !all {
		shown = config.PendingRequests(reqs)
	}
	if asJSON {
		return jsonOut(w, shown)
	}
	if len(shown) == 0 {
		fmt.Fprintln(w, "no pending petitions")
		return nil
	}
	sort.Slice(shown, func(i, j int) bool { return shown[i].At.Before(shown[j].At) })
	for _, r := range shown {
		kind := r.Kind
		if kind == "" {
			kind = config.RequestTool
		}
		mark := " "
		if r.Denied {
			mark = "✗"
		}
		spec := ""
		if kind == config.RequestServer {
			if r.URL != "" {
				spec = " · " + r.URL
			} else if r.Command != "" {
				spec = " · " + r.Command
			}
		}
		fmt.Fprintf(w, "%s %-6s %-32s %s%s\n", mark, kind, r.Name, age(r.At), spec)
		if r.Reason != "" {
			fmt.Fprintf(w, "        reason: %s\n", r.Reason)
		}
		if r.Denied && r.DenyReason != "" {
			fmt.Fprintf(w, "        denied: %s\n", r.DenyReason)
		}
	}
	fmt.Fprintln(w, "\ngrant:  toolhost requests --accept <name>")
	fmt.Fprintln(w, "deny:   toolhost requests --deny <name> --reason …")
	return nil
}

// acceptRequest grants one petition. Tool kind → the normal approve path
// (schema-pinned); server kind → the proposed spec lands in config as a
// backend, then the human approves its tools like any other.
func acceptRequest(ctx context.Context, path, name string, w io.Writer) error {
	reqs, err := config.LoadRequests(path)
	if err != nil {
		return err
	}
	var r *config.Request
	for i := range reqs {
		if reqs[i].Name == name && !reqs[i].Denied {
			r = &reqs[i]
			break
		}
	}
	if r == nil {
		return fmt.Errorf("no pending petition %q", name)
	}
	if r.Kind == config.RequestServer {
		f, err := config.Load(path)
		if err != nil {
			return err
		}
		if _, dup := f.Backends[name]; dup {
			_ = config.RemoveRequest(path, name)
			return fmt.Errorf("backend %q already exists — petition cleared", name)
		}
		b := &config.Backend{Transport: r.Transport, URL: r.URL, Command: r.Command, Args: r.Args}
		if err := f.SetBackend(name, b); err != nil {
			return err
		}
		if err := f.Save(path); err != nil {
			return err
		}
		if err := config.RemoveRequest(path, name); err != nil {
			return err
		}
		fmt.Fprintf(w, "added backend %s (%s) — live within ~2s\n", name, b.Transport)
		fmt.Fprintf(w, "next: toolhost discover, then approve its tools\n")
		return nil
	}
	return EditApprovals(ctx, path, []string{name}, true, false, w)
}

func age(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
