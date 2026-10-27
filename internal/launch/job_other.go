//go:build !windows

package launch

import (
	"errors"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

type groupProc struct {
	cmd     *exec.Cmd
	mu      sync.Mutex
	pgid    int      // non-zero only while this Proc owns the process-group identity
	waiting bool     // cmd.Wait is active; it observed ProcessState == nil while holding mu
	owner   *os.File // watchdog pipe: EOF means the play-host process died
}

// Start runs the process in its own process group. This is also the lifetime
// boundary for descendants: launchd signals the LaunchAgent leader on unload,
// but does not make child cleanup a service invariant, so the owned pgid is.
func Start(s Spec) (Proc, error) {
	c := exec.Command(s.Path, s.Args...)
	c.Dir = s.Dir
	if len(s.Env) > 0 {
		c.Env = MergeEnv(os.Environ(), s.Env, false)
	}
	if s.LogPath != "" {
		f, err := os.Create(s.LogPath)
		if err != nil {
			return nil, err
		}
		c.Stdout, c.Stderr = f, f
	}
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := c.Start(); err != nil {
		return nil, err
	}
	// Setpgid makes the child PID its pgid. Keep that identity only until
	// Wait retires it; signalling it later could hit an unrelated reused pgid.
	owner, err := startOwnerWatchdog(c.Process.Pid)
	if err != nil {
		_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
		_, _ = c.Process.Wait()
		return nil, err
	}
	return &groupProc{cmd: c, pgid: c.Process.Pid, owner: owner}, nil
}

// startOwnerWatchdog keeps the process-group cleanup invariant even if the
// host itself receives SIGKILL. The watchdog has its own group and blocks on a
// pipe owned by play-host. A clean Wait/Kill writes "done"; host death closes
// every writer without data, so the watchdog kills only the captured pgid.
func startOwnerWatchdog(pgid int) (*os.File, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	watch := exec.Command("/bin/sh", "-c", `if IFS= read -r status; then exit 0; fi; /bin/kill -KILL -- -"$1" 2>/dev/null || true`, "play-host-watchdog", strconv.Itoa(pgid))
	watch.Stdin = r
	watch.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := watch.Start(); err != nil {
		r.Close()
		w.Close()
		return nil, err
	}
	r.Close()
	go func() { _ = watch.Wait() }()
	return w, nil
}

func (p *groupProc) disarmOwnerLocked() {
	if p.owner == nil {
		return
	}
	_, _ = p.owner.WriteString("done\n")
	_ = p.owner.Close()
	p.owner = nil
}

func (p *groupProc) Pid() int { return p.cmd.Process.Pid }

func (p *groupProc) Wait() (int, error) {
	p.mu.Lock()
	if p.cmd.ProcessState != nil {
		p.pgid = 0
		p.disarmOwnerLocked()
		p.mu.Unlock()
		return p.cmd.ProcessState.ExitCode(), nil
	}
	p.waiting = true
	p.mu.Unlock()

	err := p.cmd.Wait()
	p.mu.Lock()
	p.pgid = 0
	p.waiting = false
	p.disarmOwnerLocked()
	p.mu.Unlock()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}

// Kill terminates the whole owned process group, including supervisor and
// ffmpeg descendants. It never signals after Wait has observed exit: pgids
// are reusable kernel identifiers, not durable process ownership handles.
func (p *groupProc) Kill() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pgid == 0 {
		p.disarmOwnerLocked()
		return nil
	}
	// When Wait is active, it checked ProcessState while holding mu before
	// os/exec began writing it. Avoid racing that write; pgid ownership is
	// retired under this same mutex immediately after Wait returns.
	if !p.waiting && p.cmd.ProcessState != nil {
		p.pgid = 0
		p.disarmOwnerLocked()
		return nil
	}
	pgid := p.pgid
	err := syscall.Kill(-pgid, syscall.SIGKILL)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		p.pgid = 0
		p.disarmOwnerLocked()
		return nil
	}
	return err
}

// Pids returns a point-in-time snapshot of the owned process group. Both
// Darwin and the supported development Unix hosts provide pgrep -g.
func (p *groupProc) Pids() ([]int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pgid == 0 {
		return nil, nil
	}
	if !p.waiting && p.cmd.ProcessState != nil {
		p.pgid = 0
		p.disarmOwnerLocked()
		return nil, nil
	}
	out, err := exec.Command("pgrep", "-g", strconv.Itoa(p.pgid)).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			return nil, nil
		}
		return nil, err
	}
	var pids []int
	for _, field := range strings.Fields(string(out)) {
		pid, err := strconv.Atoi(field)
		if err != nil {
			return nil, err
		}
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	return pids, nil
}
