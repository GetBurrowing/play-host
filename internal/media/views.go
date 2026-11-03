package media

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/pion/webrtc/v4"
)

// Encoder is the part of Pipeline a ViewSet drives; *Pipeline satisfies it.
type Encoder interface {
	Start(w, h int) error
	WriteFrame([]byte) error
	Running() bool
	Size() (int, int)
	Stop()
}

var _ Encoder = (*Pipeline)(nil)

// SharedView is view id 0: the shared encoder, present as long as the set
// exists. Slot s is view SlotView(s) = 1+s.
const SharedView = 0

// SlotView maps a player slot to its view id.
func SlotView(slot int) int { return 1 + slot }

// ErrEncoderCap is returned when a slot encoder refuses to start while the
// shared one runs: the driver's concurrent encode session cap. The driver is
// the oracle; no cap number is assumed.
type ErrEncoderCap struct {
	Running int // encoders already running, shared included
	Err     error
}

func (e *ErrEncoderCap) Error() string {
	return fmt.Sprintf("per-slot encoder %d refused (concurrent encode session cap on this driver): %v", e.Running+1, e.Err)
}

func (e *ErrEncoderCap) Unwrap() error { return e.Err }

type view struct {
	enc   Encoder
	track *webrtc.TrackLocalStaticRTP
}

// ViewSet is one encoder per joined player slot plus the shared one, each
// with its own track.
type ViewSet struct {
	newPipeline func(view int) (Encoder, *webrtc.TrackLocalStaticRTP)
	dropped     atomic.Int64

	mu    sync.Mutex
	views map[int]view
}

// NewViewSet creates the set with its shared view.
func NewViewSet(newPipeline func(view int) (Encoder, *webrtc.TrackLocalStaticRTP)) *ViewSet {
	vs := &ViewSet{newPipeline: newPipeline, views: map[int]view{}}
	enc, tr := newPipeline(SharedView)
	vs.views[SharedView] = view{enc, tr}
	return vs
}

// Ensure creates slot's view (idempotent). Nothing is started until its
// first frame.
func (vs *ViewSet) Ensure(slot int) {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	id := SlotView(slot)
	if _, ok := vs.views[id]; ok {
		return
	}
	enc, tr := vs.newPipeline(id)
	vs.views[id] = view{enc, tr}
}

// Drop stops and removes slot's view; other views are untouched.
func (vs *ViewSet) Drop(slot int) {
	vs.mu.Lock()
	id := SlotView(slot)
	v, ok := vs.views[id]
	delete(vs.views, id)
	vs.mu.Unlock()
	if ok && v.enc != nil {
		v.enc.Stop()
	}
}

// Track is view's track, nil when the view does not exist.
func (vs *ViewSet) Track(id int) *webrtc.TrackLocalStaticRTP {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	return vs.views[id].track
}

// Dropped counts frames sent to a view with no pipeline.
func (vs *ViewSet) Dropped() int64 { return vs.dropped.Load() }

// Write routes one w x h RGBA frame to view id, starting that view's encoder
// on its first frame. A frame for an unknown view is dropped and counted.
func (vs *ViewSet) Write(id, w, h int, frame []byte) error {
	vs.mu.Lock()
	v, ok := vs.views[id]
	if !ok || v.enc == nil {
		vs.mu.Unlock()
		vs.dropped.Add(1)
		return nil
	}
	if !v.enc.Running() {
		if err := v.enc.Start(w, h); err != nil {
			running, shared := 0, false
			for _, o := range vs.views {
				if o.enc != nil && o.enc.Running() {
					running++
				}
			}
			if sv := vs.views[SharedView]; sv.enc != nil {
				shared = sv.enc.Running()
			}
			vs.mu.Unlock()
			if id != SharedView && shared {
				return &ErrEncoderCap{Running: running, Err: err}
			}
			return err
		}
	}
	vs.mu.Unlock()
	return v.enc.WriteFrame(frame)
}

// StopAll stops every encoder.
func (vs *ViewSet) StopAll() {
	vs.mu.Lock()
	var encs []Encoder
	for _, v := range vs.views {
		if v.enc != nil {
			encs = append(encs, v.enc)
		}
	}
	vs.mu.Unlock()
	for _, e := range encs {
		e.Stop()
	}
}
