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

// An invalid-config record counts only while its gateway lives. The gateway here is a
// real second process, killed without a chance to clean up, so the test
// sees what the kernel does with its lock.
func TestInvalidRecordEndsWithItsProcess(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "toolhost.json")

	child := exec.Command(os.Args[0], "-test.run=^TestInvalidHelperProcess$")
	child.Env = append(os.Environ(), "TOOLHOST_INVALID_HELPER="+cfgPath)
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

	got := ReportedInvalid(cfgPath)
	if len(got) != 1 || got[0].PID != child.Process.Pid || got[0].Err != "parse config: fixture" || got[0].Listen != "127.0.0.1:1" {
		t.Fatalf("ReportedInvalid with the gateway running = %+v, want its record", got)
	}

	if err := child.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	if got := ReportedInvalid(cfgPath); len(got) != 0 {
		t.Fatalf("ReportedInvalid after the gateway was killed = %+v, want none", got)
	}
	record := filepath.Join(invalidDir(cfgPath), strconv.Itoa(child.Process.Pid)+".json")
	if _, err := os.Stat(record); err != nil {
		t.Fatalf("precondition: the killed gateway's record is still on disk: %v", err)
	}

	// The next gateway to mark the config sweeps the dead record.
	m, err := MarkInvalid(cfgPath, Invalid{Since: time.Now(), Err: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Errorf("dead record survived the sweep: %v", err)
	}
	if got := ReportedInvalid(cfgPath); len(got) != 1 || got[0].PID != os.Getpid() {
		t.Errorf("ReportedInvalid = %+v, want only this process", got)
	}
	m.Clear()
	if got := ReportedInvalid(cfgPath); len(got) != 0 {
		t.Errorf("ReportedInvalid after Clear = %+v, want none", got)
	}
	left, _ := filepath.Glob(filepath.Join(invalidDir(cfgPath), strconv.Itoa(os.Getpid())+".*"))
	if len(left) != 0 {
		t.Errorf("Clear left %v behind", left)
	}
}

// TestInvalidHelperProcess is the gateway for the test above: it marks the
// config invalid, says so, and waits to be killed.
func TestInvalidHelperProcess(t *testing.T) {
	cfgPath := os.Getenv("TOOLHOST_INVALID_HELPER")
	if cfgPath == "" {
		t.Skip("helper process only")
	}
	if _, err := MarkInvalid(cfgPath, Invalid{Since: time.Now(), Err: "parse config: fixture", Listen: "127.0.0.1:1"}); err != nil {
		os.Stdout.WriteString("error: " + err.Error() + "\n")
		os.Exit(1)
	}
	os.Stdout.WriteString("marked\n")
	time.Sleep(time.Minute)
	os.Exit(0)
}

// Two configs in one folder share a state dir; each sees only its own
// gateways.
func TestInvalidRecordsAreKeyedByConfig(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.json"), filepath.Join(dir, "b.json")
	m, err := MarkInvalid(a, Invalid{Since: time.Now(), Err: "x"})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Clear()
	if got := ReportedInvalid(a); len(got) != 1 {
		t.Errorf("ReportedInvalid(a) = %+v, want one", got)
	}
	if got := ReportedInvalid(b); len(got) != 0 {
		t.Errorf("ReportedInvalid(b) = %+v, want none", got)
	}
}

// A stdio gateway spawned in a project folder names its config relative to
// that folder. Its record must still be found from anywhere else.
func TestInvalidRecordNamesTheConfigAbsolutely(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, ".toolhost", "toolhost.json")
	t.Chdir(dir)
	m, err := MarkInvalid(filepath.Join(".toolhost", "toolhost.json"), Invalid{Since: time.Now(), Err: "x"})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Clear()
	t.Chdir(t.TempDir())
	if got := ReportedInvalid(cfgPath); len(got) != 1 || !filepath.IsAbs(got[0].Config) {
		t.Errorf("ReportedInvalid from another folder = %+v, want the record with an absolute path", got)
	}
}
