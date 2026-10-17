package build

import (
	"context"
	"fmt"
	"time"

	"github.com/Startup-Suite/play-host/internal/launch"
)

// ImportArgs is the headless import command line. The user args after "--"
// carry the session marker so editor-godot-loop.ps1 never reads the import
// as a GUI editor (it would already skip it for --headless; the marker makes
// the reason explicit and survives a loop edit). The marker is emitted in
// both spellings: that loop script lives on the play machine, knows only
// --suite-play-session=, and is not updated by deploying this binary.
func ImportArgs(dir, sessionID string) []string {
	return append([]string{"--headless", "--import", "--path", dir, "--"}, launch.AddonArg("session", sessionID)...)
}

// Import runs `godot --headless --import` in its own Job Object, calling
// progress every tick until it exits, and killing the job's tree (and only
// it) when timeout or ctx expires.
func Import(ctx context.Context, godot, dir, sessionID, logPath string, timeout, tick time.Duration, progress func(time.Duration)) error {
	p, err := launch.Start(launch.Spec{Path: godot, Args: ImportArgs(dir, sessionID), Dir: dir, LogPath: logPath})
	if err != nil {
		return fmt.Errorf("start import: %w", err)
	}
	type res struct {
		code int
		err  error
	}
	done := make(chan res, 1)
	go func() {
		c, e := p.Wait()
		done <- res{c, e}
	}()
	t0 := time.Now()
	tk := time.NewTicker(tick)
	defer tk.Stop()
	limit := time.NewTimer(timeout)
	defer limit.Stop()
	for {
		select {
		case r := <-done:
			_ = p.Kill() // releases the job handle; the tree has already exited
			if r.err != nil {
				return fmt.Errorf("import wait: %w", r.err)
			}
			if r.code != 0 {
				return fmt.Errorf("import exited %d (see %s)", r.code, logPath)
			}
			return nil
		case <-tk.C:
			if progress != nil {
				progress(time.Since(t0))
			}
		case <-limit.C:
			_ = p.Kill()
			<-done
			return fmt.Errorf("import timed out after %s", timeout)
		case <-ctx.Done():
			_ = p.Kill()
			<-done
			return ctx.Err()
		}
	}
}
