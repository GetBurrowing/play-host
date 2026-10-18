//go:build !windows

package launch

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if pidFile := os.Getenv("PLAY_HOST_CRASH_HELPER"); pidFile != "" {
		p, err := Start(Spec{Path: "/bin/sh", Args: []string{"-c", "sleep 30 & wait"}})
		if err != nil {
			_ = os.WriteFile(pidFile, []byte("start: "+err.Error()), 0o600)
			os.Exit(2)
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if pids, err := p.Pids(); err == nil && len(pids) >= 2 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		_ = os.WriteFile(pidFile, []byte(strconv.Itoa(p.Pid())), 0o600)
		select {}
	}
	os.Exit(m.Run())
}

func TestHostSIGKILLWatchdogRemovesOwnedGroup(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "owned-pgid")
	host := exec.Command(os.Args[0], "-test.run=^$")
	host.Env = append(os.Environ(), "PLAY_HOST_CRASH_HELPER="+pidFile)
	if err := host.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = host.Process.Kill(); _, _ = host.Process.Wait() }()

	var pgid int
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(pidFile)
		if err == nil {
			pgid, err = strconv.Atoi(string(b))
			if err != nil {
				t.Fatalf("helper failed: %s", b)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pgid == 0 {
		t.Fatal("helper did not publish its owned pgid")
	}
	defer func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) }()
	if err := host.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_, _ = host.Process.Wait()

	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	pids, _ := exec.Command("pgrep", "-g", strconv.Itoa(pgid)).Output()
	t.Fatalf("owned process group %d survived host SIGKILL: %s", pgid, pids)
}

func waitForGroup(t *testing.T, p Proc, n int) []int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		pids, err := p.Pids()
		if err != nil {
			t.Fatal(err)
		}
		if len(pids) >= n {
			return pids
		}
		time.Sleep(25 * time.Millisecond)
	}
	pids, err := p.Pids()
	t.Fatalf("process group never reached %d members: pids=%v err=%v", n, pids, err)
	return nil
}

func TestKillStopsTreeAndLogsOutput(t *testing.T) {
	log := filepath.Join(t.TempDir(), "out.log")
	p, err := Start(Spec{Path: "/bin/sh", Args: []string{"-c", "echo started; sleep 30 & wait"}, LogPath: log})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _, _ = p.Wait(); close(done) }()
	pids := waitForGroup(t, p, 2)
	if !slices.Contains(pids, p.Pid()) {
		t.Fatalf("group snapshot %v omitted leader %d", pids, p.Pid())
	}
	if err := p.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := p.Kill(); err != nil {
		t.Fatalf("second Kill must be idempotent: %v", err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("tree survived Kill")
	}
	if got, err := p.Pids(); err != nil || len(got) != 0 {
		t.Fatalf("retired group still reported: pids=%v err=%v", got, err)
	}
	b, _ := os.ReadFile(log)
	if !strings.Contains(string(b), "started") {
		t.Fatalf("log %q", b)
	}
}

func TestKillAfterWaitNeverSignalsRetiredGroup(t *testing.T) {
	p, err := Start(Spec{Path: "/bin/sh", Args: []string{"-c", "exit 0"}})
	if err != nil {
		t.Fatal(err)
	}
	if code, err := p.Wait(); err != nil || code != 0 {
		t.Fatalf("Wait = %d, %v", code, err)
	}
	gp := p.(*groupProc)
	if gp.pgid != 0 || gp.cmd.ProcessState == nil {
		t.Fatalf("Wait did not retire ownership: pgid=%d state=%v", gp.pgid, gp.cmd.ProcessState)
	}
	if err := p.Kill(); err != nil {
		t.Fatalf("Kill after Wait = %v", err)
	}
}

func TestGodotArgsCarryMarkerAfterDoubleDash(t *testing.T) {
	a := GodotArgsForDriver(`C:\co\01a0db5f-0123456789ab`, "vulkan", 1280, 720, "sess-1", 40320, 60)
	got := strings.Join(a, " ")
	want := `--path C:\co\01a0db5f-0123456789ab --rendering-driver vulkan --resolution 1280x720 --windowed -- --burrow-play-session=sess-1 --burrow-play-port=40320 --burrow-play-fps=60 --suite-play-session=sess-1 --suite-play-port=40320 --suite-play-fps=60`
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
	for _, bad := range []string{"--remote-debug", "--editor", "-e"} {
		for _, x := range a {
			if x == bad {
				t.Fatalf("%s in play args", bad)
			}
		}
	}
}

// The new prefix comes first and every legacy arg follows it: an older addon
// copy reads only --suite-play-*, and the play machine's editor loop knows a
// played Godot by its --suite-play-session= marker.
func TestAddonArgsEmitBurrowPrefixThenLegacy(t *testing.T) {
	got := AddonArgs("s1", 40320, 30)
	want := []string{
		"--burrow-play-session=s1", "--burrow-play-port=40320", "--burrow-play-fps=30",
		"--suite-play-session=s1", "--suite-play-port=40320", "--suite-play-fps=30",
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("got  %v\nwant %v", got, want)
	}
	if a := AddonArg("export", "image"); strings.Join(a, " ") != "--burrow-play-export=image --suite-play-export=image" {
		t.Fatalf("AddonArg = %v", a)
	}
}
