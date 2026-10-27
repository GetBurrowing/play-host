// Package unreal builds an Unreal Engine checkout into a packaged Windows
// game with RunUAT BuildCookRun, caches the result by commit, and describes
// how to launch it. RunUAT runs under the session's Job Object (launch.Start),
// so stopping the session kills UnrealBuildTool and ShaderCompileWorker too.
//
// Progress rides the existing building status detail: the detail starts with
// the exact step word (Preparing, Compiling, Cooking, Staging, Packaging,
// Archiving). UAT gives no percentage, so none is reported.
package unreal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Startup-Suite/play-host/internal/capture"
	"github.com/Startup-Suite/play-host/internal/launch"
)

// Steps, in build order. The detail of a building status starts with one.
const (
	StepPreparing = "Preparing"
	StepEditor    = "Editor scripts"
	StepCompiling = "Compiling"
	StepCooking   = "Cooking"
	StepStaging   = "Staging"
	StepPackaging = "Packaging"
	StepArchiving = "Archiving"
)

// Steps is every step word, for consumers that recognise a detail.
var Steps = []string{StepPreparing, StepEditor, StepCompiling, StepCooking, StepStaging, StepPackaging, StepArchiving}

// DefaultTimeout bounds one build; a first cook is long, so it is generous.
const DefaultTimeout = 3600 * time.Second

// MaxErrBytes caps the failure line in the status detail.
const MaxErrBytes = 300

// Config is the machine's Unreal setup. Paths come from config.json; this
// repository only ever holds placeholders.
type Config struct {
	UERoot        string        // the engine install root
	ArchiveRoot   string        // packaged builds, <root>/<repo>/<sha40>/Windows/<Project>.exe
	DDCDir        string        // persistent derived-data cache; never under CheckoutsDir
	SharedDDC     string        // optional UE-SharedDataCachePath
	CheckoutsDir  string        // where session checkouts live (deleted after each session)
	Git           string        // git executable ("" = git)
	Timeout       time.Duration // 0 = DefaultTimeout
	ProgressEvery time.Duration // 0 = 5 s
	// NoArchiveCache builds even when a complete archive for the commit
	// exists (the dry-run's warm pass). The finished build still replaces it.
	NoArchiveCache bool
}

// Validate checks what a build needs, and that the DDC outlives checkouts.
func (c Config) Validate() error {
	switch {
	case c.UERoot == "":
		return errors.New("ue_root is not configured")
	case c.ArchiveRoot == "":
		return errors.New("archive_root is not configured")
	case c.DDCDir == "":
		return errors.New("ddc_dir is not configured")
	}
	if c.CheckoutsDir != "" && within(c.DDCDir, c.CheckoutsDir) {
		return errors.New("ddc_dir must not be under the checkouts dir: checkouts are deleted after every session")
	}
	return nil
}

func within(path, root string) bool {
	p, err1 := filepath.Abs(path)
	r, err2 := filepath.Abs(root)
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(r, p)
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}

// Input is one build.
type Input struct {
	RepoKey string // a path-safe name for the repository
	SHA     string // 40 hex
	Dir     string // the checkout
	LogPath string // the session log RunUAT writes to
}

// Builder runs builds. The zero Run, Start and Command are the real ones.
type Builder struct {
	Cfg  Config
	Logf func(string, ...any)
	// Run executes a helper command (git lfs ...) and returns its output.
	Run func(ctx context.Context, dir, name string, args ...string) ([]byte, error)
	// Start launches RunUAT in a Job Object.
	Start func(launch.Spec) (launch.Proc, error)
	// Command turns RunUAT.bat and its args into the process to start.
	Command func(uat string, args []string) (string, []string)
	// EditorCommand turns UnrealEditor-Cmd.exe and its args into the process
	// to start (the identity by default; tests swap in a helper).
	EditorCommand func(exe string, args []string) (string, []string)
	// Env is the session identity the editor runner's guard checks. The runner
	// refuses to start the editor without it: an editor started in session 0
	// or from a service has no desktop and no GPU context.
	Env capture.Env
	// ExtraEnv is added to RunUAT's environment after the DDC entries (tests).
	ExtraEnv []string
	// Poll is how often the log is read (0 = 100 ms).
	Poll time.Duration
}

func (b *Builder) logf(format string, a ...any) {
	if b.Logf != nil {
		b.Logf(format, a...)
	}
}

func (b *Builder) run(ctx context.Context, dir, name string, args ...string) ([]byte, error) {
	if b.Run != nil {
		return b.Run(ctx, dir, name, args...)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	return cmd.CombinedOutput()
}

func (b *Builder) start(s launch.Spec) (launch.Proc, error) {
	if b.Start != nil {
		return b.Start(s)
	}
	return launch.Start(s)
}

func (b *Builder) command(uat string, args []string) (string, []string) {
	if b.Command != nil {
		return b.Command(uat, args)
	}
	if runtime.GOOS == "windows" {
		sh := os.Getenv("ComSpec")
		if sh == "" {
			sh = "cmd.exe"
		}
		return sh, append([]string{"/c", uat}, args...)
	}
	return uat, args
}

// FindProject returns the single .uproject at dir's root and its name.
func FindProject(dir string) (path, name string, err error) {
	m, err := filepath.Glob(filepath.Join(dir, "*.uproject"))
	if err != nil {
		return "", "", err
	}
	if len(m) != 1 {
		return "", "", fmt.Errorf("want exactly one .uproject at the checkout root, found %d", len(m))
	}
	abs, err := filepath.Abs(m[0])
	if err != nil {
		return "", "", err
	}
	return abs, strings.TrimSuffix(filepath.Base(abs), ".uproject"), nil
}

// ArchiveDir is where the finished build for sha lives.
func (c Config) ArchiveDir(repoKey, sha string) string {
	return filepath.Join(c.ArchiveRoot, repoKey, sha)
}

// ExePath is the packaged executable inside an archive directory.
func ExePath(archiveDir, project string) string {
	return filepath.Join(archiveDir, "Windows", project+".exe")
}

const completeMarker = ".complete"

func reusable(archiveDir, project string) (string, bool) {
	exe := ExePath(archiveDir, project)
	if _, err := os.Stat(filepath.Join(archiveDir, completeMarker)); err != nil {
		return "", false
	}
	if st, err := os.Stat(exe); err != nil || st.IsDir() {
		return "", false
	}
	return exe, true
}

// UATArgs is the BuildCookRun command line after RunUAT.bat.
func UATArgs(uproject, archiveDir string) []string {
	return []string{
		"BuildCookRun", "-project=" + uproject, "-noP4", "-utf8output", "-unattended",
		"-platform=Win64", "-clientconfig=Development",
		"-build", "-cook", "-stage", "-pak", "-archive", "-archivedirectory=" + archiveDir,
	}
}

// Env is the DDC environment for RunUAT.
func (c Config) Env() []string {
	env := []string{"UE-LocalDataCachePath=" + c.DDCDir}
	if c.SharedDDC != "" {
		env = append(env, "UE-SharedDataCachePath="+c.SharedDDC)
	}
	return env
}

// Build returns the packaged executable for in, building it unless a
// complete archive for the commit exists. progress receives a detail that
// starts with the current step word.
func (b *Builder) Build(ctx context.Context, in Input, progress func(string)) (string, error) {
	if err := b.Cfg.Validate(); err != nil {
		return "", err
	}
	uproject, name, err := FindProject(in.Dir)
	if err != nil {
		return "", err
	}
	final := b.Cfg.ArchiveDir(in.RepoKey, in.SHA)
	if exe, ok := reusable(final, name); ok && !b.Cfg.NoArchiveCache {
		progress("Reusing build " + in.SHA[:12])
		b.logf("unreal: reusing archived build %s", final)
		return exe, nil
	}

	// The manifest is checked before anything slow runs, so a bad one fails
	// in seconds, not after the LFS pull.
	jobs, err := PlanEditorJobs(in.Dir, uproject)
	if err != nil {
		return "", err
	}

	git := b.Cfg.Git
	if git == "" {
		git = "git"
	}
	if _, err := b.run(ctx, in.Dir, git, "lfs", "version"); err != nil {
		return "", errors.New("git lfs missing on the play host")
	}
	progress(StepPreparing + ": pulling Git LFS objects")
	if out, err := b.run(ctx, in.Dir, git, "lfs", "install", "--local"); err != nil {
		return "", fmt.Errorf("git lfs install: %w: %s", err, tail(string(out), 200))
	}
	if out, err := b.run(ctx, in.Dir, git, "lfs", "pull"); err != nil {
		return "", fmt.Errorf("git lfs pull: %w: %s", err, tail(string(out), 200))
	}

	if err := b.runEditorJobs(ctx, in, uproject, jobs, progress); err != nil {
		return "", err
	}

	partial := final + ".partial"
	for _, d := range []string{final, partial} {
		if err := os.RemoveAll(d); err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(partial, 0o755); err != nil {
		return "", err
	}
	if err := os.MkdirAll(b.Cfg.DDCDir, 0o755); err != nil {
		return "", err
	}
	if err := b.runUAT(ctx, in, uproject, partial, progress); err != nil {
		_ = os.RemoveAll(partial)
		return "", err
	}
	if st, err := os.Stat(ExePath(partial, name)); err != nil || st.IsDir() {
		_ = os.RemoveAll(partial)
		return "", fmt.Errorf("Build failed during %s: packaged executable %s.exe not found", StepArchiving, name)
	}
	if err := os.WriteFile(filepath.Join(partial, completeMarker), []byte(in.SHA+"\n"), 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(partial, final); err != nil {
		return "", err
	}
	return ExePath(final, name), nil
}

func (b *Builder) runUAT(ctx context.Context, in Input, uproject, archiveDir string, progress func(string)) error {
	uat := filepath.Join(b.Cfg.UERoot, "Engine", "Build", "BatchFiles", "RunUAT.bat")
	path, args := b.command(uat, UATArgs(uproject, archiveDir))
	spec := launch.Spec{Path: path, Args: args, Dir: in.Dir, LogPath: in.LogPath, Env: append(b.Cfg.Env(), b.ExtraEnv...)}
	b.logf("unreal: RunUAT %v", UATArgs(uproject, archiveDir))
	return b.runJob(ctx, spec, "RunUAT", StepPreparing, true, progress)
}

// runJob runs spec under the Job Object and follows its log: banners (UAT
// only) move the step, and a non-zero exit, a timeout or a stop ends it. The
// step word in progress and in the failure text is the scanner's current one.
func (b *Builder) runJob(ctx context.Context, spec launch.Spec, label, startStep string, banners bool, progress func(string)) error {
	timeout := b.Cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	every := b.Cfg.ProgressEvery
	if every <= 0 {
		every = 5 * time.Second
	}
	poll := b.Poll
	if poll <= 0 {
		poll = 100 * time.Millisecond
	}
	proc, err := b.start(spec)
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}

	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	sc := &scanner{path: spec.LogPath, step: startStep, banners: banners}
	t0 := time.Now()
	report := func() {
		progress(fmt.Sprintf("%s (%ds)", sc.current(), int(time.Since(t0).Seconds())))
	}
	report()

	type exit struct {
		code int
		err  error
	}
	done := make(chan exit, 1)
	go func() {
		c, err := proc.Wait()
		done <- exit{c, err}
	}()
	tickLog, tickProg := time.NewTicker(poll), time.NewTicker(every)
	defer tickLog.Stop()
	defer tickProg.Stop()

	var ex exit
	killed := false
loop:
	for {
		select {
		case ex = <-done:
			break loop
		case <-tickLog.C:
			if sc.read() {
				report()
			}
		case <-tickProg.C:
			report()
		case <-tctx.Done():
			killed = true
			_ = proc.Kill()
			ex = <-done
			break loop
		}
	}
	sc.read()
	if killed {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("Build timed out during %s", sc.current())
	}
	if ex.err != nil {
		return fmt.Errorf("%s: %w", label, ex.err)
	}
	if ex.code != 0 || sc.exit > 0 {
		code := ex.code
		if code == 0 {
			code = sc.exit
		}
		line := sc.failureLine()
		if line == "" {
			line = "AutomationTool exited with code " + strconv.Itoa(code)
		}
		return fmt.Errorf("Build failed during %s: %s", sc.current(), cut(line, MaxErrBytes))
	}
	return nil
}

// banners maps UAT's "COMMAND STARTED" banners to steps.
var banners = []struct {
	re   *regexp.Regexp
	step string
}{
	{regexp.MustCompile(`\bBUILD COMMAND STARTED\b`), StepCompiling},
	{regexp.MustCompile(`\bCOOK COMMAND STARTED\b`), StepCooking},
	{regexp.MustCompile(`\bSTAGE COMMAND STARTED\b`), StepStaging},
	{regexp.MustCompile(`\bPACKAGE COMMAND STARTED\b`), StepPackaging},
	{regexp.MustCompile(`\bARCHIVE COMMAND STARTED\b`), StepArchiving},
}

var exitRe = regexp.MustCompile(`AutomationTool exiting with ExitCode=(-?\d+)`)
var errRe = regexp.MustCompile(`(?i)\b(error|fatal|failed)\b`)

// scanner follows the growing UAT log.
type scanner struct {
	banners bool // follow UAT's COMMAND STARTED banners and exit line
	path    string
	off     int64
	buf     []byte

	mu      sync.Mutex
	step    string
	exit    int // from "AutomationTool exiting with ExitCode=N"
	lastErr string
	last    string
}

func (s *scanner) current() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.step
}

func (s *scanner) failureLine() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastErr != "" {
		return s.lastErr
	}
	return s.last
}

// read consumes new log bytes and reports whether the step changed.
func (s *scanner) read() bool {
	f, err := os.Open(s.path)
	if err != nil {
		return false
	}
	defer f.Close()
	if _, err := f.Seek(s.off, io.SeekStart); err != nil {
		return false
	}
	data, _ := io.ReadAll(f)
	s.off += int64(len(data))
	s.buf = append(s.buf, data...)
	changed := false
	for {
		i := bytes.IndexByte(s.buf, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimSpace(string(s.buf[:i]))
		s.buf = s.buf[i+1:]
		if s.line(line) {
			changed = true
		}
	}
	return changed
}

func (s *scanner) line(line string) bool {
	if line == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last = line
	if !s.banners {
		if errRe.MatchString(line) {
			s.lastErr = line
		}
		return false
	}
	if m := exitRe.FindStringSubmatch(line); m != nil {
		s.exit, _ = strconv.Atoi(m[1])
		return false
	}
	for _, b := range banners {
		if b.re.MatchString(line) {
			if s.step == b.step {
				return false
			}
			s.step = b.step
			return true
		}
	}
	if errRe.MatchString(line) {
		s.lastErr = line
	}
	return false
}

// LaunchSpec is how the archived game is started. Stage 1's capture finds its
// window; the engine writes its own log.
func LaunchSpec(exe string, w, h int, logPath string) launch.Spec {
	return launch.Spec{
		Path: exe,
		Args: []string{"-windowed", "-ResX=" + strconv.Itoa(w), "-ResY=" + strconv.Itoa(h), "-WinX=0", "-WinY=0", "-nosplash", "-log"},
		Dir:  filepath.Dir(exe), LogPath: logPath,
	}
}

// RepoKey makes a path-safe name from a repository match string.
func RepoKey(match string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			return r
		}
		return '_'
	}, match)
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// cut shortens s to at most n bytes on a rune boundary.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	c := n
	for c > 0 && !utf8.RuneStart(s[c]) {
		c--
	}
	return s[:c]
}
