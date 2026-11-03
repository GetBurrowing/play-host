package link

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/Startup-Suite/play-host/internal/probe"
)

// Record kinds and fixed PCM v1 format written by addons/burrow_play.
const (
	KindImageRGBA8 = 1 // get_viewport().get_texture().get_image()
	KindRDTexture  = 2 // RenderingDevice.texture_get_data_async
	PCMFormatF32LE = 1 // interleaved IEEE-754 float32, little-endian
	PCMSampleRate  = 48000
	PCMChannels    = 2
	MaxPCMFrames   = 4800 // 100 ms; bounds one record and corrupted lengths
)

// FrameHeader precedes every exported frame on the addon link. View is 0 for
// SPF1 and for an SPF2 shared-view frame; an SPF2 slot frame carries 1+slot.
type FrameHeader struct {
	Width, Height, Kind, Length uint32
	View                        uint32
}

// MaxView is the highest SPF2 view id (1+slot for slots 0..31).
const MaxView = 32

// ErrBadRecord marks a record that is malformed but whose extent is known:
// the reader has already consumed it, so the stream is still in sync and the
// link need not fail.
var ErrBadRecord = errors.New("bad record")

var (
	frameMagic  = [4]byte{'S', 'P', 'F', '1'}
	frameMagic2 = [4]byte{'S', 'P', 'F', '2'}
	pcmMagic    = [4]byte{'S', 'P', 'A', '1'}
)

// ReadFrameHeader reads one "SPF1" (20 byte) or "SPF2" (24 byte) header. An
// out-of-range SPF2 view returns the parsed header and an ErrBadRecord error
// after consuming the header, so the caller can skip Length payload bytes.
func ReadFrameHeader(r io.Reader) (FrameHeader, error) {
	var b [24]byte
	if _, err := io.ReadFull(r, b[:20]); err != nil {
		return FrameHeader{}, err
	}
	v2 := false
	switch [4]byte(b[:4]) {
	case frameMagic:
	case frameMagic2:
		v2 = true
		if _, err := io.ReadFull(r, b[20:]); err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return FrameHeader{}, err
		}
	default:
		return FrameHeader{}, fmt.Errorf("bad frame magic %q", b[:4])
	}
	h := FrameHeader{
		Width:  binary.LittleEndian.Uint32(b[4:]),
		Height: binary.LittleEndian.Uint32(b[8:]),
		Kind:   binary.LittleEndian.Uint32(b[12:]),
		Length: binary.LittleEndian.Uint32(b[16:]),
	}
	if v2 {
		h.View = binary.LittleEndian.Uint32(b[20:])
	}
	if h.Width == 0 || h.Height == 0 || h.Width > 7680 || h.Height > 4320 {
		return h, fmt.Errorf("implausible frame size %dx%d", h.Width, h.Height)
	}
	if h.Length < h.Width*h.Height*4 || h.Length > h.Width*h.Height*16 {
		return h, fmt.Errorf("frame length %d does not fit %dx%d", h.Length, h.Width, h.Height)
	}
	if h.View > MaxView {
		return h, fmt.Errorf("%w: implausible view %d", ErrBadRecord, h.View)
	}
	return h, nil
}

// WriteFrameHeader is the Go mirror of the addon's SPF1 writer (used by
// tests). Its bytes are fixed; View is ignored.
func WriteFrameHeader(w io.Writer, h FrameHeader) error {
	var b [20]byte
	copy(b[:4], frameMagic[:])
	binary.LittleEndian.PutUint32(b[4:], h.Width)
	binary.LittleEndian.PutUint32(b[8:], h.Height)
	binary.LittleEndian.PutUint32(b[12:], h.Kind)
	binary.LittleEndian.PutUint32(b[16:], h.Length)
	_, err := w.Write(b[:])
	return err
}

// WriteFrameHeader2 is the Go mirror of the addon's SPF2 writer.
func WriteFrameHeader2(w io.Writer, h FrameHeader) error {
	var b [24]byte
	copy(b[:4], frameMagic2[:])
	binary.LittleEndian.PutUint32(b[4:], h.Width)
	binary.LittleEndian.PutUint32(b[8:], h.Height)
	binary.LittleEndian.PutUint32(b[12:], h.Kind)
	binary.LittleEndian.PutUint32(b[16:], h.Length)
	binary.LittleEndian.PutUint32(b[20:], h.View)
	_, err := w.Write(b[:])
	return err
}

// PCMHeader precedes one version-1 game-audio record. The wire layout is
// "SPA1" followed by five little-endian u32 values: sample rate, channels,
// sample format, sample frames and payload bytes. Version 1 is deliberately
// fixed to 48 kHz stereo f32le so a project cannot silently change the track
// clock or channel layout underneath a running WebRTC sender.
type PCMHeader struct {
	SampleRate, Channels, Format, Frames, Length uint32
}

// ReadPCMHeader reads and validates one SPA1 header.
func ReadPCMHeader(r io.Reader) (PCMHeader, error) {
	var b [24]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return PCMHeader{}, err
	}
	if [4]byte(b[:4]) != pcmMagic {
		return PCMHeader{}, fmt.Errorf("bad pcm magic %q", b[:4])
	}
	h := PCMHeader{
		SampleRate: binary.LittleEndian.Uint32(b[4:]),
		Channels:   binary.LittleEndian.Uint32(b[8:]),
		Format:     binary.LittleEndian.Uint32(b[12:]),
		Frames:     binary.LittleEndian.Uint32(b[16:]),
		Length:     binary.LittleEndian.Uint32(b[20:]),
	}
	if h.SampleRate != PCMSampleRate || h.Channels != PCMChannels || h.Format != PCMFormatF32LE {
		return h, fmt.Errorf("unsupported pcm %d Hz/%d ch/format %d", h.SampleRate, h.Channels, h.Format)
	}
	if h.Frames == 0 || h.Frames > MaxPCMFrames || h.Length != h.Frames*h.Channels*4 {
		return h, fmt.Errorf("pcm length %d does not fit %d frames x %d channels f32", h.Length, h.Frames, h.Channels)
	}
	return h, nil
}

// WritePCMHeader is the Go mirror of the addon's writer (used by tests).
func WritePCMHeader(w io.Writer, h PCMHeader) error {
	var b [24]byte
	copy(b[:4], pcmMagic[:])
	binary.LittleEndian.PutUint32(b[4:], h.SampleRate)
	binary.LittleEndian.PutUint32(b[8:], h.Channels)
	binary.LittleEndian.PutUint32(b[12:], h.Format)
	binary.LittleEndian.PutUint32(b[16:], h.Frames)
	binary.LittleEndian.PutUint32(b[20:], h.Length)
	_, err := w.Write(b[:])
	return err
}

// ErrShortFrame means the buffer cannot hold the probe corner.
var ErrShortFrame = errors.New("frame smaller than probe corner")

// DecodeCornerRGBA reads the probe code from a tightly packed RGBA8 frame by
// sampling each cell's centre pixel.
func DecodeCornerRGBA(pix []byte, width int) (uint16, error) {
	edge := probe.Cells * probe.CellPx
	if width < edge || len(pix) < width*4*edge {
		return 0, ErrShortFrame
	}
	var bits [probe.Cells * probe.Cells]bool
	for i := range bits {
		x := (i%probe.Cells)*probe.CellPx + probe.CellPx/2
		y := (i/probe.Cells)*probe.CellPx + probe.CellPx/2
		o := (y*width + x) * 4
		lum := (299*int(pix[o]) + 587*int(pix[o+1]) + 114*int(pix[o+2])) / 1000
		bits[i] = lum >= 128
	}
	return probe.Decode(bits)
}
