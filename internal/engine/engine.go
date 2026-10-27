// Package engine picks the game engine for one checkout from its contents, so
// play_session_start does not change: the engine is a property of the build.
package engine

import (
	"errors"
	"os"
	"path/filepath"
)

// Kind is a game engine the host can serve.
type Kind string

const (
	Godot  Kind = "godot"
	Unreal Kind = "unreal"
)

// ErrAmbiguous is returned when the checkout names no engine, or several.
var ErrAmbiguous = errors.New("checkout has both/neither project.godot and .uproject")

// Detect returns godot if the root has project.godot, unreal if the root has
// exactly one *.uproject, and ErrAmbiguous otherwise (neither, both, or more
// than one .uproject).
func Detect(dir string) (Kind, error) {
	godot := false
	if st, err := os.Stat(filepath.Join(dir, "project.godot")); err == nil && !st.IsDir() {
		godot = true
	}
	uprojects, err := filepath.Glob(filepath.Join(dir, "*.uproject"))
	if err != nil {
		return "", err
	}
	switch {
	case godot && len(uprojects) == 0:
		return Godot, nil
	case !godot && len(uprojects) == 1:
		return Unreal, nil
	}
	return "", ErrAmbiguous
}
