package link

import (
	"bytes"
	"io"
	"testing"

	"github.com/Startup-Suite/play-host/internal/probe"
)

func paint(seq uint16, w, h int) []byte {
	pix := make([]byte, w*h*4)
	bits := probe.Encode(seq)
	for i, on := range bits {
		if !on {
			continue
		}
		cx, cy := (i%probe.Cells)*probe.CellPx, (i/probe.Cells)*probe.CellPx
		for y := cy; y < cy+probe.CellPx; y++ {
			for x := cx; x < cx+probe.CellPx; x++ {
				o := (y*w + x) * 4
				pix[o], pix[o+1], pix[o+2], pix[o+3] = 255, 255, 255, 255
			}
		}
	}
	return pix
}

func TestDecodeCorner(t *testing.T) {
	for _, seq := range []uint16{1, 77, 4095} {
		got, err := DecodeCornerRGBA(paint(seq, 128, 80), 128)
		if err != nil || got != seq {
			t.Fatalf("seq %d: got %d %v", seq, got, err)
		}
	}
	if _, err := DecodeCornerRGBA(make([]byte, 128*80*4), 128); err != probe.ErrNoCode {
		t.Fatalf("black corner: %v", err)
	}
	if _, err := DecodeCornerRGBA(make([]byte, 10), 2); err != ErrShortFrame {
		t.Fatal("short frame accepted")
	}
}

func TestFrameHeaderRoundTrip(t *testing.T) {
	var b bytes.Buffer
	in := FrameHeader{Width: 1280, Height: 720, Kind: KindImageRGBA8, Length: 1280 * 720 * 4}
	if err := WriteFrameHeader(&b, in); err != nil {
		t.Fatal(err)
	}
	out, err := ReadFrameHeader(&b)
	if err != nil || out != in {
		t.Fatalf("%+v %v", out, err)
	}
	b.Reset()
	_ = WriteFrameHeader(&b, FrameHeader{Width: 1280, Height: 720, Kind: 1, Length: 12})
	if _, err := ReadFrameHeader(&b); err == nil {
		t.Fatal("short length accepted")
	}
	if _, err := ReadFrameHeader(bytes.NewReader([]byte("XXXX0000000000000000"))); err == nil {
		t.Fatal("bad magic accepted")
	}
}

func TestSPF2RoundTrip(t *testing.T) {
	for _, view := range []uint32{0, 1, 17, MaxView} {
		var b bytes.Buffer
		in := FrameHeader{Width: 640, Height: 360, Kind: KindImageRGBA8, Length: 640 * 360 * 4, View: view}
		if err := WriteFrameHeader2(&b, in); err != nil {
			t.Fatal(err)
		}
		if b.Len() != 24 {
			t.Fatalf("SPF2 header is %d bytes", b.Len())
		}
		out, err := ReadFrameHeader(&b)
		if err != nil || out != in {
			t.Fatalf("view %d: %+v %v", view, out, err)
		}
	}
}

func TestSPF1UnchangedBytes(t *testing.T) {
	var b bytes.Buffer
	if err := WriteFrameHeader(&b, FrameHeader{Width: 64, Height: 48, Kind: 1, Length: 12288, View: 9}); err != nil {
		t.Fatal(err)
	}
	want := []byte{'S', 'P', 'F', '1', 64, 0, 0, 0, 48, 0, 0, 0, 1, 0, 0, 0, 0, 0x30, 0, 0}
	if !bytes.Equal(b.Bytes(), want) {
		t.Fatalf("SPF1 bytes % x, want % x", b.Bytes(), want)
	}
	h, err := ReadFrameHeader(&b)
	if err != nil || h.View != 0 {
		t.Fatalf("%+v %v", h, err)
	}
}

func TestSPF2TruncatedHeader(t *testing.T) {
	var b bytes.Buffer
	_ = WriteFrameHeader2(&b, FrameHeader{Width: 64, Height: 48, Kind: 1, Length: 12288, View: 2})
	for _, n := range []int{22, 20} { // inside the view field; exactly the SPF1 length
		if _, err := ReadFrameHeader(bytes.NewReader(b.Bytes()[:n])); err != io.ErrUnexpectedEOF {
			t.Fatalf("%d bytes: %v", n, err)
		}
	}
}
