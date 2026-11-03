package media

import (
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/pion/webrtc/v4"
)

type fakeEnc struct {
	mu      sync.Mutex
	id      int
	failing error
	running bool
	starts  int
	stops   int
	frames  int
}

func (f *fakeEnc) Start(w, h int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failing != nil {
		return f.failing
	}
	f.running, f.starts = true, f.starts+1
	return nil
}
func (f *fakeEnc) WriteFrame([]byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.frames++
	return nil
}
func (f *fakeEnc) Running() bool    { f.mu.Lock(); defer f.mu.Unlock(); return f.running }
func (f *fakeEnc) Size() (int, int) { return 0, 0 }
func (f *fakeEnc) Stop()            { f.mu.Lock(); f.running, f.stops = false, f.stops+1; f.mu.Unlock() }
func (f *fakeEnc) counts() (int, int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts, f.stops, f.frames
}

func newFakeSet() (*ViewSet, map[int]*fakeEnc) {
	encs := map[int]*fakeEnc{}
	vs := NewViewSet(func(view int) (Encoder, *webrtc.TrackLocalStaticRTP) {
		f := &fakeEnc{id: view}
		encs[view] = f
		tr, _ := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeH264, ClockRate: 90000}, "video", "v")
		return f, tr
	})
	return vs, encs
}

func TestViewSetSharedAlwaysPresent(t *testing.T) {
	vs, encs := newFakeSet()
	if encs[SharedView] == nil || vs.Track(SharedView) == nil {
		t.Fatal("shared view missing right after construction")
	}
	if vs.Track(SlotView(0)) != nil {
		t.Fatal("slot view exists before Ensure")
	}
	vs.Drop(0)
	if vs.Track(SharedView) == nil {
		t.Fatal("dropping a slot removed the shared view")
	}
}

func TestViewSetEnsureIdempotentOwnTracks(t *testing.T) {
	vs, encs := newFakeSet()
	vs.Ensure(0)
	first := encs[1]
	tr := vs.Track(1)
	vs.Ensure(0)
	if encs[1] != first || vs.Track(1) != tr {
		t.Fatal("second Ensure replaced the view")
	}
	vs.Ensure(1)
	if vs.Track(1) == vs.Track(2) || vs.Track(1) == vs.Track(SharedView) {
		t.Fatal("views share a track")
	}
}

func TestViewSetDropStopsExactlyThatView(t *testing.T) {
	vs, encs := newFakeSet()
	vs.Ensure(0)
	vs.Ensure(1)
	for _, id := range []int{0, 1, 2} {
		if err := vs.Write(id, 8, 8, []byte{1}); err != nil {
			t.Fatal(err)
		}
	}
	vs.Drop(0)
	for id, want := range map[int]int{0: 0, 1: 1, 2: 0} {
		if _, stops, _ := encs[id].counts(); stops != want {
			t.Fatalf("view %d stops = %d, want %d", id, stops, want)
		}
	}
	if vs.Track(1) != nil || vs.Track(2) == nil {
		t.Fatal("wrong view removed")
	}
}

func TestViewSetWriteRoutesByViewAndDropsUnknown(t *testing.T) {
	vs, encs := newFakeSet()
	vs.Ensure(0)
	vs.Ensure(1)
	for range 2 {
		vs.Write(SharedView, 8, 8, []byte{1})
	}
	vs.Write(1, 8, 8, []byte{1})
	vs.Write(2, 8, 8, []byte{1})
	for id, want := range map[int]int{0: 2, 1: 1, 2: 1} {
		if _, _, fr := encs[id].counts(); fr != want {
			t.Fatalf("view %d frames = %d, want %d", id, fr, want)
		}
	}
	if st, _, _ := encs[1].counts(); st != 1 {
		t.Fatalf("lazy start count = %d", st)
	}
	if err := vs.Write(9, 8, 8, []byte{1}); err != nil || vs.Dropped() != 1 {
		t.Fatalf("unknown view: err=%v dropped=%d", err, vs.Dropped())
	}
}

func TestViewSetEncoderCapShape(t *testing.T) {
	vs, encs := newFakeSet()
	vs.Ensure(0)
	vs.Ensure(1)
	vs.Write(SharedView, 8, 8, []byte{1})
	vs.Write(1, 8, 8, []byte{1})
	boom := errors.New("OpenEncodeSessionEx failed: out of memory")
	vs.Ensure(2)
	encs[3].failing = boom
	err := vs.Write(3, 8, 8, []byte{1})
	var cap *ErrEncoderCap
	if !errors.As(err, &cap) || !errors.Is(err, boom) || cap.Running != 2 {
		t.Fatalf("err = %v", err)
	}
	want := "per-slot encoder 3 refused (concurrent encode session cap on this driver): " + boom.Error()
	if err.Error() != want {
		t.Fatalf("detail %q, want %q", err.Error(), want)
	}
	// Shared failing is a plain error, not a cap.
	vs2, e2 := newFakeSet()
	e2[SharedView].failing = boom
	if err := vs2.Write(SharedView, 8, 8, nil); errors.As(err, &cap) || err == nil {
		t.Fatalf("shared failure = %v", err)
	}
}

func TestPipelineRTPPortZeroResolvesEphemeralPort(t *testing.T) {
	exe, _ := os.Executable()
	t.Setenv("FAKE_FFMPEG", "1")
	s := &sink{}
	p := &Pipeline{FFmpeg: exe, Preset: ControlPreset(), RTPPort: 0, Track: s}
	if err := p.Start(16, 16); err != nil {
		t.Fatal(err)
	}
	defer p.Stop()
	joined := strings.Join(p.Args, " ")
	i := strings.Index(joined, "rtp://127.0.0.1:")
	if i < 0 || strings.Contains(joined, "rtp://127.0.0.1:0") {
		t.Fatalf("args lack a resolved port: %s", joined)
	}
	p.WriteFrame(make([]byte, 16*16*4))
	waitN(t, s, 1)
}
