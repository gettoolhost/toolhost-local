//go:build darwin || linux || freebsd || netbsd || openbsd || dragonfly

package config

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// A degraded record counts only while its gateway lives. The gateway here is a
// real second process, killed without a chance to clean up, so the test
// sees what the kernel does with its lock.
func TestDegradedRecordEndsWithItsProcess(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "toolhost.json")

	child := exec.Command(os.Args[0], "-test.run=^TestDegradedHelperProcess$")
	child.Env = append(os.Environ(), "TOOLHOST_DEGRADED_HELPER="+cfgPath)
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "marked\n" {
		t.Fatalf("helper: %q, %v", line, err)
	}

	got := ReportedDegraded(cfgPath)
	if len(got) != 1 || got[0].PID != child.Process.Pid || got[0].ConfigError == nil || got[0].ConfigError.Error != "parse config: fixture" || got[0].Listen != "127.0.0.1:1" {
		t.Fatalf("ReportedDegraded with the gateway running = %+v, want its record", got)
	}

	if err := child.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	if got := ReportedDegraded(cfgPath); len(got) != 0 {
		t.Fatalf("ReportedDegraded after the gateway was killed = %+v, want none", got)
	}
	record := filepath.Join(degradedDir(cfgPath), strconv.Itoa(child.Process.Pid)+".json")
	if _, err := os.Stat(record); err != nil {
		t.Fatalf("precondition: the killed gateway's record is still on disk: %v", err)
	}

	// The next gateway to mark the config sweeps the dead record.
	m, err := MarkDegraded(cfgPath, Degraded{ConfigError: &Fault{Since: time.Now(), Error: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Errorf("dead record survived the sweep: %v", err)
	}
	if got := ReportedDegraded(cfgPath); len(got) != 1 || got[0].PID != os.Getpid() {
		t.Errorf("ReportedDegraded = %+v, want only this process", got)
	}
	m.Clear()
	if got := ReportedDegraded(cfgPath); len(got) != 0 {
		t.Errorf("ReportedDegraded after Clear = %+v, want none", got)
	}
	left, _ := filepath.Glob(filepath.Join(degradedDir(cfgPath), strconv.Itoa(os.Getpid())+".*"))
	if len(left) != 0 {
		t.Errorf("Clear left %v behind", left)
	}
}

// TestDegradedHelperProcess is the gateway for the test above: it marks the
// config degraded, says so, and waits to be killed.
func TestDegradedHelperProcess(t *testing.T) {
	cfgPath := os.Getenv("TOOLHOST_DEGRADED_HELPER")
	if cfgPath == "" {
		t.Skip("helper process only")
	}
	if _, err := MarkDegraded(cfgPath, Degraded{Listen: "127.0.0.1:1", ConfigError: &Fault{Since: time.Now(), Error: "parse config: fixture"}}); err != nil {
		os.Stdout.WriteString("error: " + err.Error() + "\n")
		os.Exit(1)
	}
	os.Stdout.WriteString("marked\n")
	time.Sleep(time.Minute)
	os.Exit(0)
}

// Two configs in one folder share a state dir; each sees only its own
// gateways.
func TestDegradedRecordsAreKeyedByConfig(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.json"), filepath.Join(dir, "b.json")
	m, err := MarkDegraded(a, Degraded{ConfigError: &Fault{Since: time.Now(), Error: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Clear()
	if got := ReportedDegraded(a); len(got) != 1 {
		t.Errorf("ReportedDegraded(a) = %+v, want one", got)
	}
	if got := ReportedDegraded(b); len(got) != 0 {
		t.Errorf("ReportedDegraded(b) = %+v, want none", got)
	}
}

// A stdio gateway spawned in a project folder names its config relative to
// that folder. Its record must still be found from anywhere else.
func TestDegradedRecordNamesTheConfigAbsolutely(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, ".toolhost", "toolhost.json")
	t.Chdir(dir)
	m, err := MarkDegraded(filepath.Join(".toolhost", "toolhost.json"), Degraded{ConfigError: &Fault{Since: time.Now(), Error: "x"}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Clear()
	t.Chdir(t.TempDir())
	if got := ReportedDegraded(cfgPath); len(got) != 1 || !filepath.IsAbs(got[0].Config) {
		t.Errorf("ReportedDegraded from another folder = %+v, want the record with an absolute path", got)
	}
}

// Both faults survive the round trip through the record.
func TestDegradedRecordCarriesBothFaults(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "toolhost.json")
	now := time.Now()
	m, err := MarkDegraded(cfgPath, Degraded{ConfigError: &Fault{Since: now, Error: "parse"}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Clear()
	if err := m.Update(Degraded{ConfigError: &Fault{Since: now, Error: "parse"}, ResolveError: &Fault{Since: now, Error: "clash"}}); err != nil {
		t.Fatal(err)
	}
	got := ReportedDegraded(cfgPath)
	if len(got) != 1 || got[0].ConfigError == nil || got[0].ResolveError == nil || got[0].ResolveError.Error != "clash" || !got[0].ResolveError.Since.Equal(now) {
		t.Errorf("ReportedDegraded = %+v, want both faults", got)
	}
}
