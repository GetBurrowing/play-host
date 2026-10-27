package input

import (
	"encoding/json"
	"testing"
)

// The addon lines for engine=godot are byte-for-byte what they were before
// Unreal input existed: the injector reads these lines, it never changes them.
func TestGodotInputLinesUnchanged(t *testing.T) {
	b := NewV1Binding()
	var got []string
	for _, m := range []string{
		`{"t":"key","code":"KeyW","down":true}`,
		`{"t":"key","code":"ShiftRight","down":true}`,
		`{"t":"touch","src":0,"id":0,"ph":"down","x":0.25,"y":0.75}`,
		`{"t":"pad","src":0,"seq":1,"connected":true,"buttons":[1,0,0,0,0,0,0.5,0,0,0,0,0,0,0,0,0,0],"axes":[0.5,0,0,0]}`,
		`{"t":"probe","seq":7}`,
	} {
		r, err := b.Handle("t", []byte(m))
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range r.Lines {
			j, _ := json.Marshal(l)
			got = append(got, string(j))
		}
	}
	want := []string{
		`{"d":0,"e":false,"k":"W","loc":0,"p":true,"t":"key"}`,
		`{"d":0,"e":false,"k":"Shift","loc":2,"p":true,"t":"key"}`,
		`{"c":false,"d":0,"i":0,"p":true,"t":"st","x":0.25,"y":0.75}`,
		`{"b":0,"d":0,"p":true,"t":"jb","v":1}`,
		`{"a":4,"d":0,"t":"ja","v":0.5}`,
		`{"a":0,"d":0,"t":"ja","v":0.5}`,
		`{"seq":7,"t":"probe"}`,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d lines:\n%v", len(got), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d:\n got %s\nwant %s", i, got[i], want[i])
		}
	}
}
