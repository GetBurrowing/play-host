package launch

import (
	"strings"
	"testing"
)

func TestMergeEnvReplacesDuplicates(t *testing.T) {
	base := []string{"A=1", "Path=x", "FAKE=1", "B=2"}
	got := strings.Join(MergeEnv(base, []string{"FAKE=0", "NEW=3"}, false), " ")
	if got != "A=1 Path=x FAKE=0 B=2 NEW=3" {
		t.Fatalf("got %s", got)
	}
	// Windows names are case-insensitive: path replaces Path, once.
	got = strings.Join(MergeEnv(base, []string{"PATH=y"}, true), " ")
	if got != "A=1 PATH=y FAKE=1 B=2" {
		t.Fatalf("fold: %s", got)
	}
	// A name already repeated in base collapses to one value.
	got = strings.Join(MergeEnv([]string{"X=1", "X=2"}, []string{"X=3"}, false), " ")
	if got != "X=3" {
		t.Fatalf("dups: %s", got)
	}
}
