//go:build windows

package launch

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// These run ON HOST-A (GOOS=windows go test -c, then the .exe there). They need
// PLAY_HOST_TEST_GODOT = the Godot *_console.exe. PLAY_HOST_SIBLING_PIDS may
// list pids that must survive (e.g. the editor agent's headless editor); the test only
// ever OBSERVES those, it never signals them.

func alive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	ev, _ := windows.WaitForSingleObject(h, 0)
	return ev == uint32(windows.WAIT_TIMEOUT)
}

func miniProject(t *testing.T) string {
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "project.godot"), []byte("config_version=5\n\n[application]\n\nconfig/name=\"play-host-kill-test-01a0db5f\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestJobKillTakesOnlyItsOwnTree(t *testing.T) {
	godot := os.Getenv("PLAY_HOST_TEST_GODOT")
	if godot == "" {
		t.Skip("PLAY_HOST_TEST_GODOT not set")
	}
	proj := miniProject(t)
	// Pre-existing processes the test did not start.
	var external []int
	for _, s := range strings.Split(os.Getenv("PLAY_HOST_SIBLING_PIDS"), ",") {
		if pid, err := strconv.Atoi(strings.TrimSpace(s)); err == nil && pid > 0 {
			if !alive(pid) {
				t.Fatalf("sibling pid %d is not alive before the test; the check would prove nothing", pid)
			}
			external = append(external, pid)
		}
	}
	// A sibling Godot started OUTSIDE any job the launcher makes.
	sib := exec.Command(godot, "--headless", "--path", proj, "--", "--burrow-play-session=sibling-01a0db5f")
	if err := sib.Start(); err != nil {
		t.Fatal(err)
	}
	defer sib.Process.Kill()

	// The tree under test, through the launcher's Job Object.
	p, err := Start(Spec{Path: godot, Args: []string{"--headless", "--path", proj, "--", "--burrow-play-session=job-01a0db5f"}, Dir: proj, LogPath: filepath.Join(t.TempDir(), "godot.log")})
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	end := time.Now().Add(15 * time.Second)
	for time.Now().Before(end) {
		pids, _ = p.Pids()
		if len(pids) >= 2 { // console wrapper + the real exe it spawns
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(pids) < 2 {
		t.Fatalf("job holds %v; expected the console wrapper and its child", pids)
	}
	time.Sleep(time.Second)
	if !alive(sib.Process.Pid) {
		t.Fatal("sibling died before the kill; nothing is being tested")
	}
	if err := p.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := p.Kill(); err != nil { // idempotent
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	for _, pid := range pids {
		if alive(pid) {
			t.Errorf("job pid %d survived Kill", pid)
		}
	}
	if !alive(sib.Process.Pid) {
		t.Errorf("sibling Godot %d (not in the job) was killed", sib.Process.Pid)
	}
	for _, pid := range external {
		if !alive(pid) {
			t.Errorf("external pid %d was killed", pid)
		}
	}
	t.Logf("job pids killed %v; sibling %d alive; external %v alive", pids, sib.Process.Pid, external)
}

func TestKillOnJobCloseCoversChildOfChild(t *testing.T) {
	// cmd /c starts ping as a grandchild of the launcher; the job holds both.
	p, err := Start(Spec{Path: `C:\Windows\System32\cmd.exe`, Args: []string{"/c", `C:\Windows\System32\PING.EXE -n 60 127.0.0.1`}})
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	end := time.Now().Add(5 * time.Second)
	for time.Now().Before(end) {
		pids, _ = p.Pids()
		if len(pids) >= 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(pids) < 2 {
		t.Fatalf("job %v", pids)
	}
	p.Kill()
	time.Sleep(time.Second)
	for _, pid := range pids {
		if alive(pid) {
			t.Errorf("pid %d survived", pid)
		}
	}
}
