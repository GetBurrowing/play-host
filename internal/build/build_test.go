package build

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// exampleGameProject is ExampleGame's project.godot at bebe173f (2026-09-26).
const exampleGameProject = `; Engine configuration file.
config_version=5

[application]

config/name="ExampleGame"
run/main_scene="res://main/main.tscn"
config/features=PackedStringArray("4.7", "Forward Plus")

[autoload]

_mcp_game_helper="*res://addons/godot_ai/runtime/game_helper.gd"

[editor_plugins]

enabled=PackedStringArray("res://addons/godot_ai/plugin.cfg")

[physics]

3d/physics_engine="Jolt Physics"
`

func TestPatchProjectGodotExampleGame(t *testing.T) {
	out, removed := PatchProjectGodot(exampleGameProject)
	if strings.Contains(out, "_mcp_game_helper") || strings.Contains(out, "godot_ai") {
		t.Fatalf("godot_ai survived:\n%s", out)
	}
	if strings.Count(out, Autoload) != 1 {
		t.Fatalf("autoload not added once:\n%s", out)
	}
	if !strings.Contains(out, "[autoload]\n\n"+Autoload+"\n\n[editor_plugins]") {
		t.Fatalf("autoload placement:\n%s", out)
	}
	if !strings.Contains(out, "enabled=PackedStringArray()") {
		t.Fatalf("editor plugin list:\n%s", out)
	}
	for _, keep := range []string{`run/main_scene="res://main/main.tscn"`, `3d/physics_engine="Jolt Physics"`, "config_version=5"} {
		if !strings.Contains(out, keep) {
			t.Errorf("lost %q", keep)
		}
	}
	if len(removed) != 2 {
		t.Fatalf("removed %v", removed)
	}
	// Idempotent: patching twice changes nothing.
	again, removed2 := PatchProjectGodot(out)
	if again != out || len(removed2) != 0 {
		t.Fatalf("not idempotent:\n%s\n---\n%s", out, again)
	}
}

func TestPatchProjectGodotKeepsOtherPluginsAndAddsSection(t *testing.T) {
	src := "config_version=5\r\n\r\n[editor_plugins]\r\n\r\nenabled=PackedStringArray(\"res://addons/other/plugin.cfg\", \"res://addons/godot_ai/plugin.cfg\")\r\n"
	out, removed := PatchProjectGodot(src)
	if !strings.Contains(out, `enabled=PackedStringArray("res://addons/other/plugin.cfg")`) || len(removed) != 1 {
		t.Fatalf("out %s removed %v", out, removed)
	}
	if !strings.HasSuffix(out, "[autoload]\n\n"+Autoload+"\n") {
		t.Fatalf("no autoload section appended:\n%q", out)
	}
}

func TestNormalizeAndFindRepo(t *testing.T) {
	for _, u := range []string{
		"https://github.com/example-owner/example-game", "https://github.com/Example-Owner/example-game.git",
		"git@github.com:example-owner/example-game.git", "ssh://git@github.com/example-owner/example-game.git",
		"https://x-token@github.com/example-owner/example-game/", "ssh://git@github.com:22/example-owner/example-game",
	} {
		n, err := NormalizeRepo(u)
		if err != nil || n != "github.com/example-owner/example-game" {
			t.Errorf("%s -> %q %v", u, n, err)
		}
	}
	for _, bad := range []string{"", "example-game", "https://github.com/example-owner", "https://github.com/a/b/c"} {
		if _, err := NormalizeRepo(bad); err == nil {
			t.Errorf("%q normalized", bad)
		}
	}
	c := Config{Repos: []Repo{{Match: "github.com/example-owner/example-game", CloneURL: "git@github.com:example-owner/example-game.git"}}}
	if r, err := c.FindRepo("https://github.com/example-owner/example-game"); err != nil || r.CloneURL == "" {
		t.Fatal(r, err)
	}
	if _, err := c.FindRepo("https://github.com/evil/example-game"); err == nil {
		t.Fatal("unlisted repo accepted")
	}
}

func TestCheckoutNaming(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	if got := Task8("01a0db5f-4f1a-7001-87a3-e777ad1cab82", "", "s"); got != "01a0db5f" {
		t.Fatal(got)
	}
	if got := Task8("", "task/01a0dbd6-76bb-7001-a322-08a81e3d7927", "s"); got != "01a0dbd6" {
		t.Fatal(got)
	}
	if got := Task8("", "main", "Sess-1234-5678-xyz"); got != "sess1234" {
		t.Fatal(got)
	}
	d, err := CheckoutDir(filepath.FromSlash("/c/checkouts"), "01a0db5f", sha)
	if err != nil || d != filepath.Join(filepath.FromSlash("/c/checkouts"), "01a0db5f-0123456789ab") {
		t.Fatal(d, err)
	}
	for _, bad := range []struct{ t8, sha string }{{"01a0db5f", "abc"}, {"../x", sha}, {"", sha}, {"01a0db5f", strings.ToUpper(sha)}} {
		if _, err := CheckoutDir("/c", bad.t8, bad.sha); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func testGit(t *testing.T) string {
	t.Helper()
	if g := os.Getenv("PLAY_HOST_TEST_GIT"); g != "" {
		return g
	}
	g, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	return g
}

func run(t *testing.T, git, dir string, args ...string) string {
	t.Helper()
	c := exec.Command(git, append([]string{"-C", dir}, args...)...)
	c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestMirrorWorktreeExactSha builds a real upstream with two commits on a
// branch, then checks the builder checks out the OLDER sha it was asked for
// (never the branch head), asserts HEAD, reuses the checkout, and refuses a
// checkout that has drifted.
func TestMirrorWorktreeExactSha(t *testing.T) {
	git := testGit(t)
	root := t.TempDir()
	up := filepath.Join(root, "upstream")
	os.MkdirAll(up, 0o755)
	run(t, git, up, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(up, "project.godot"), []byte(exampleGameProject), 0o644)
	run(t, git, up, "add", ".")
	run(t, git, up, "commit", "-q", "-m", "one")
	want := run(t, git, up, "rev-parse", "HEAD")
	os.WriteFile(filepath.Join(up, "later.txt"), []byte("2"), 0o644)
	run(t, git, up, "add", ".")
	run(t, git, up, "commit", "-q", "-m", "two")
	head := run(t, git, up, "rev-parse", "HEAD")

	addon := filepath.Join(root, "addon")
	os.MkdirAll(addon, 0o755)
	os.WriteFile(filepath.Join(addon, "burrow_play.gd"), []byte("extends Node\n"), 0o644)

	b := &Builder{Cfg: Config{
		Git: git, MirrorsDir: filepath.Join(root, "mirrors"), CheckoutsDir: filepath.Join(root, "checkouts"), AddonDir: addon,
		Repos: []Repo{{Match: "github.com/o/example-game", CloneURL: up}},
	}}
	r, _ := b.Cfg.FindRepo("https://github.com/o/example-game")
	ctx := context.Background()
	mirror, err := b.EnsureCommit(ctx, r, want)
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := CheckoutDir(b.Cfg.CheckoutsDir, "01a0db5f", want)
	if err := b.Checkout(ctx, r, mirror, dir, want); err != nil {
		t.Fatal(err)
	}
	if got := run(t, git, dir, "rev-parse", "HEAD"); got != want || got == head {
		t.Fatalf("checked out %s, want %s (branch head %s)", got, want, head)
	}
	if _, err := os.Stat(filepath.Join(dir, "later.txt")); err == nil {
		t.Fatal("checkout carries the later commit")
	}
	installed, removed, err := b.InstallAddon(dir)
	if err != nil || len(removed) != 2 || installed != BurrowPlay {
		t.Fatal(installed, removed, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "addons", "burrow_play", "burrow_play.gd")); err != nil {
		t.Fatal(err)
	}
	// Re-checkout of the same sha reuses the dir (keeps the import cache).
	os.WriteFile(filepath.Join(dir, "marker"), nil, 0o644)
	if err := b.Checkout(ctx, r, mirror, dir, want); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "marker")); err != nil {
		t.Fatal("same-sha checkout was not reused")
	}
	// Drift: someone moves HEAD; the assertion must fail.
	run(t, git, dir, "checkout", "-q", "--detach", head)
	if err := b.AssertHead(ctx, dir, want); !errors.Is(err, ErrWrongHead) {
		t.Fatalf("drifted checkout passed: %v", err)
	}
	// And Checkout repairs it back to the exact sha.
	if err := b.Checkout(ctx, r, mirror, dir, want); err != nil {
		t.Fatal(err)
	}
	if got := run(t, git, dir, "rev-parse", "HEAD"); got != want {
		t.Fatalf("repair left HEAD at %s", got)
	}
	// A sha the remote has never had fails loudly, no fallback.
	if _, err := b.EnsureCommit(ctx, r, strings.Repeat("d", 40)); err == nil {
		t.Fatal("unknown sha accepted")
	}
	// A commit pushed after the mirror was made is fetched.
	os.WriteFile(filepath.Join(up, "three.txt"), []byte("3"), 0o644)
	run(t, git, up, "add", ".")
	run(t, git, up, "commit", "-q", "-m", "three")
	third := run(t, git, up, "rev-parse", "HEAD")
	if _, err := b.EnsureCommit(ctx, r, third); err != nil {
		t.Fatal(err)
	}
}

func TestImportArgsCarryMarker(t *testing.T) {
	a := strings.Join(ImportArgs("C:/co/x", "sess"), " ")
	if a != "--headless --import --path C:/co/x -- --burrow-play-session=sess --suite-play-session=sess" {
		t.Fatal(a)
	}
}

// TestRemoveCheckout (stage 6): an ended session's checkout is removed from
// disk AND from the mirror's worktree list, the mirror survives, the same
// sha checks out again afterwards, and nothing outside CheckoutsDir can be
// removed through it.
func TestRemoveCheckout(t *testing.T) {
	git := testGit(t)
	root := t.TempDir()
	up := filepath.Join(root, "upstream")
	os.MkdirAll(up, 0o755)
	run(t, git, up, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(up, "project.godot"), []byte(exampleGameProject), 0o644)
	run(t, git, up, "add", ".")
	run(t, git, up, "commit", "-q", "-m", "one")
	sha := run(t, git, up, "rev-parse", "HEAD")
	b := &Builder{Cfg: Config{
		Git: git, MirrorsDir: filepath.Join(root, "mirrors"), CheckoutsDir: filepath.Join(root, "checkouts"),
		Repos: []Repo{{Match: "github.com/o/example-game", CloneURL: up}},
	}}
	r, _ := b.Cfg.FindRepo("https://github.com/o/example-game")
	ctx := context.Background()
	mirror, err := b.EnsureCommit(ctx, r, sha)
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := CheckoutDir(b.Cfg.CheckoutsDir, "01a0db5f", sha)
	if err := b.Checkout(ctx, r, mirror, dir, sha); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(dir, ".godot", "imported"), 0o755) // an import cache
	os.WriteFile(filepath.Join(dir, ".godot", "imported", "x.ctex"), []byte("x"), 0o644)

	if err := b.RemoveCheckout(ctx, mirror, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("checkout still on disk: %v", err)
	}
	if list := run(t, git, mirror, "worktree", "list"); strings.Contains(list, filepath.Base(dir)) {
		t.Fatalf("mirror still lists the worktree:\n%s", list)
	}
	if _, err := os.Stat(mirror); err != nil {
		t.Fatalf("the mirror was removed too: %v", err)
	}
	// The next session of the same sha builds again from the kept mirror.
	if err := b.Checkout(ctx, r, mirror, dir, sha); err != nil {
		t.Fatal(err)
	}
	// Refused: CheckoutsDir itself, a path outside it, a nested path.
	outside := filepath.Join(root, "upstream")
	for _, bad := range []string{b.Cfg.CheckoutsDir, outside, filepath.Join(dir, "addons")} {
		if err := b.RemoveCheckout(ctx, mirror, bad); err == nil {
			t.Errorf("RemoveCheckout(%s) was allowed", bad)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "project.godot")); err != nil {
		t.Fatal("a refused remove deleted something")
	}
}

// TestSweepCheckouts (stage 6): at host start every checkout under
// CheckoutsDir is removed, mirror-owned or not, and the mirror is kept.
func TestSweepCheckouts(t *testing.T) {
	git := testGit(t)
	root := t.TempDir()
	up := filepath.Join(root, "upstream")
	os.MkdirAll(up, 0o755)
	run(t, git, up, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(up, "project.godot"), []byte(exampleGameProject), 0o644)
	run(t, git, up, "add", ".")
	run(t, git, up, "commit", "-q", "-m", "one")
	sha := run(t, git, up, "rev-parse", "HEAD")
	b := &Builder{Cfg: Config{
		Git: git, MirrorsDir: filepath.Join(root, "mirrors"), CheckoutsDir: filepath.Join(root, "checkouts"),
		Repos: []Repo{{Match: "github.com/o/example-game", CloneURL: up}},
	}}
	r, _ := b.Cfg.FindRepo("https://github.com/o/example-game")
	ctx := context.Background()
	mirror, _ := b.EnsureCommit(ctx, r, sha)
	dir, _ := CheckoutDir(b.Cfg.CheckoutsDir, "01a0dcc3", sha)
	if err := b.Checkout(ctx, r, mirror, dir, sha); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(b.Cfg.CheckoutsDir, "stray-dir")
	os.MkdirAll(stray, 0o755)
	removed, err := b.SweepCheckouts(ctx)
	if len(removed) != 2 {
		t.Fatalf("removed %v err %v", removed, err)
	}
	if ents, _ := os.ReadDir(b.Cfg.CheckoutsDir); len(ents) != 0 {
		t.Fatalf("left %v", ents)
	}
	if list := run(t, git, mirror, "worktree", "list"); strings.Contains(list, filepath.Base(dir)) {
		t.Fatalf("mirror still lists it:\n%s", list)
	}
	if err := b.Checkout(ctx, r, mirror, dir, sha); err != nil {
		t.Fatalf("mirror unusable after the sweep: %v", err)
	}
	b2 := &Builder{Cfg: Config{Git: git, CheckoutsDir: filepath.Join(root, "missing")}}
	if got, err := b2.SweepCheckouts(ctx); err != nil || len(got) != 0 {
		t.Fatalf("missing dir: %v %v", got, err)
	}
}

func TestDetectAddonPrefersBurrowPlayAndAcceptsLegacy(t *testing.T) {
	dir := t.TempDir()
	if _, err := DetectAddon(dir); err == nil {
		t.Fatal("an empty addon_dir was accepted")
	}
	os.WriteFile(filepath.Join(dir, "suite_play.gd"), []byte("extends Node\n"), 0o644)
	if a, err := DetectAddon(dir); err != nil || a != LegacySuitePlay {
		t.Fatalf("legacy only: %v %v", a, err)
	}
	os.WriteFile(filepath.Join(dir, "burrow_play.gd"), []byte("extends Node\n"), 0o644)
	if a, err := DetectAddon(dir); err != nil || a != BurrowPlay {
		t.Fatalf("both: %v %v", a, err)
	}
}

// A host whose addon_dir still holds the older suite_play.gd gets that
// script installed under its own name and registered with its own autoload,
// so the autoload never points at a file that is not there.
func TestInstallAddonLegacyCopy(t *testing.T) {
	root := t.TempDir()
	addon := filepath.Join(root, "addon")
	os.MkdirAll(addon, 0o755)
	os.WriteFile(filepath.Join(addon, "suite_play.gd"), []byte("extends Node\n"), 0o644)
	dir := filepath.Join(root, "co")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "project.godot"), []byte("[autoload]\n\nBurrowPlay=\"*res://addons/burrow_play/burrow_play.gd\"\n"), 0o644)
	b := &Builder{Cfg: Config{AddonDir: addon}}
	installed, _, err := b.InstallAddon(dir)
	if err != nil || installed != LegacySuitePlay {
		t.Fatal(installed, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "addons", "suite_play", "suite_play.gd")); err != nil {
		t.Fatal(err)
	}
	pg, _ := os.ReadFile(filepath.Join(dir, "project.godot"))
	if strings.Count(string(pg), "Play=") != 1 || !strings.Contains(string(pg), LegacyAutoload) {
		t.Fatalf("project.godot:\n%s", pg)
	}
}
