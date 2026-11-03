package link

import (
	"bytes"
	"errors"
	"net"
	"strings"
	"testing"
)

func pcmRecord(t *testing.T, w *bytes.Buffer) {
	t.Helper()
	h := PCMHeader{SampleRate: PCMSampleRate, Channels: PCMChannels, Format: PCMFormatF32LE, Frames: 4, Length: 32}
	if err := WritePCMHeader(w, h); err != nil {
		t.Fatal(err)
	}
	w.Write(make([]byte, 32))
}

func frameRecord(t *testing.T, w *bytes.Buffer, v2 bool, view uint32, fill byte) {
	t.Helper()
	h := FrameHeader{Width: 8, Height: 8, Kind: KindImageRGBA8, Length: 8 * 8 * 4, View: view}
	write := WriteFrameHeader
	if v2 {
		write = WriteFrameHeader2
	}
	if err := write(w, h); err != nil {
		t.Fatal(err)
	}
	w.Write(bytes.Repeat([]byte{fill}, int(h.Length)))
}

func linkOver(stream []byte) (*Link, func()) {
	a, b := net.Pipe()
	go func() { _, _ = b.Write(stream); b.Close() }()
	return New(a), func() { a.Close() }
}

func TestSPF2ViewOutOfRangeIsSkippedNotFatal(t *testing.T) {
	var s bytes.Buffer
	frameRecord(t, &s, true, 40, 0xAA) // bad: view 40
	frameRecord(t, &s, false, 0, 0x11) // good SPF1
	l, done := linkOver(s.Bytes())
	defer done()
	_, _, err := l.ReadRecord(nil)
	if !errors.Is(err, ErrBadRecord) || !strings.Contains(err.Error(), "view 40") {
		t.Fatalf("bad record: %v", err)
	}
	rec, _, err := l.ReadRecord(nil)
	if err != nil || rec.Frame == nil || rec.SPF2 || rec.Frame.View != 0 || rec.Data[0] != 0x11 || len(rec.Data) != 256 {
		t.Fatalf("record after the bad one: %+v %v", rec, err)
	}
}

func TestReadRecordMixedSPF1SPF2SPA1(t *testing.T) {
	var s bytes.Buffer
	frameRecord(t, &s, false, 0, 1)
	frameRecord(t, &s, true, 3, 2)
	pcmRecord(t, &s)
	frameRecord(t, &s, true, 0, 3)
	l, done := linkOver(s.Bytes())
	defer done()
	type want struct {
		pcm, spf2 bool
		view      uint32
		first     byte
	}
	for i, w := range []want{{false, false, 0, 1}, {false, true, 3, 2}, {true, false, 0, 0}, {false, true, 0, 3}} {
		rec, _, err := l.ReadRecord(nil)
		if err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		if w.pcm {
			if rec.PCM == nil || rec.Frame != nil {
				t.Fatalf("record %d: want pcm: %+v", i, rec)
			}
			continue
		}
		if rec.Frame == nil || rec.SPF2 != w.spf2 || rec.Frame.View != w.view || rec.Data[0] != w.first {
			t.Fatalf("record %d: %+v", i, rec)
		}
	}
}
