package app

import (
	"fmt"
	"io"
	"time"

	"github.com/gettoolhost/toolhost-local/internal/config"
)

// Audit prints the tail of the gateway's JSONL audit log — the same
// bounded read toolhost__audit exposes to agents, for the human at the
// shell. It reads the file directly: the gateway need not be running.
// limit <= 0 or > 500 reads the default tail, mirroring metaAudit.
func Audit(cfgPath string, limit int, kind string, w io.Writer) error {
	f, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	events, err := auditTail(auditLogPath(f, cfgPath), limit, kind)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		fmt.Fprintln(w, "no audit events")
		return nil
	}
	for _, ev := range events {
		fmt.Fprintf(w, "%s  %-14s", ev.TS.Format(time.RFC3339), ev.Kind)
		if ev.Backend != "" {
			fmt.Fprintf(w, "  backend=%s", ev.Backend)
		}
		if ev.Tool != "" {
			fmt.Fprintf(w, "  tool=%s", ev.Tool)
		}
		if ev.MS > 0 {
			fmt.Fprintf(w, "  %dms", ev.MS)
		}
		if ev.Remote != "" {
			fmt.Fprintf(w, "  remote=%s", ev.Remote)
		}
		if ev.Err != "" {
			fmt.Fprintf(w, "  err=%s", ev.Err)
		}
		fmt.Fprintln(w)
	}
	return nil
}
