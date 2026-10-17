// Package probe implements the input-to-photon latency probe shared by the
// play host, the Godot addon (addons/burrow_play) and the browser harness.
//
// A probe is an input event carrying a sequence number. The Godot addon draws
// that number in the top-left corner of the frame for two frames as a 4x4
// grid of 16x16-pixel cells (one H.264 macroblock per cell, so DCT and chroma
// bleed stay inside a cell). The browser timestamps the send, decodes the
// corner in requestVideoFrameCallback and takes the difference.
//
// Layout: 16 bits, row-major, most significant bit at cell (0,0). The value is
// seq<<4 | Check(seq), with seq in 1..4095. A white cell is a 1. An all-black
// corner is the idle state and never decodes, because Check(0) != 0.
//
// The same constants are duplicated in addons/burrow_play/burrow_play.gd and
// spike/web/probe.js; code_test.go pins the wire values those copies must match.
package probe

import (
	"errors"
	"math"
	"sort"
)

const (
	// Cells is the grid edge (4x4 cells = 16 bits).
	Cells = 4
	// CellPx is the edge of one cell in pixels.
	CellPx = 16
	// MaxSeq is the largest encodable sequence number.
	MaxSeq = 1<<12 - 1
)

// ErrNoCode is returned when the corner holds no valid probe code.
var ErrNoCode = errors.New("probe: no valid code in corner")

// Check is the 4-bit check nibble for seq.
func Check(seq uint16) uint16 {
	s := seq & MaxSeq
	return (s ^ s>>4 ^ s>>8 ^ 0xA) & 0xF
}

// Encode returns the 16 cell states for seq, row-major, cell (0,0) first.
func Encode(seq uint16) [Cells * Cells]bool {
	v := (seq&MaxSeq)<<4 | Check(seq)
	var bits [Cells * Cells]bool
	for i := range bits {
		bits[i] = v&(1<<(15-uint(i))) != 0
	}
	return bits
}

// Decode turns 16 cell states back into a sequence number.
func Decode(bits [Cells * Cells]bool) (uint16, error) {
	var v uint16
	for i, b := range bits {
		if b {
			v |= 1 << (15 - uint(i))
		}
	}
	seq := v >> 4
	if seq == 0 || v&0xF != Check(seq) {
		return 0, ErrNoCode
	}
	return seq, nil
}

// Summary is the p50/p95 of a latency sample, nearest-rank.
type Summary struct {
	N   int     `json:"n"`
	P50 float64 `json:"p50_ms"`
	P95 float64 `json:"p95_ms"`
	Min float64 `json:"min_ms"`
	Max float64 `json:"max_ms"`
}

// Summarize computes nearest-rank percentiles. An empty sample returns N=0.
func Summarize(ms []float64) Summary {
	if len(ms) == 0 {
		return Summary{}
	}
	s := append([]float64(nil), ms...)
	sort.Float64s(s)
	return Summary{N: len(s), P50: rank(s, 0.50), P95: rank(s, 0.95), Min: s[0], Max: s[len(s)-1]}
}

func rank(sorted []float64, p float64) float64 {
	i := int(math.Ceil(p*float64(len(sorted)))) - 1
	if i < 0 {
		i = 0
	}
	return sorted[i]
}
