// Package bench holds the pure parsing and summarising behind `play-host
// bench` (task 01a1a358 stage 4): ffmpeg children of the running play-host,
// nvidia-smi encoder utilisation and VRAM, and the per-slot input-to-photon
// summary of spike/driver/run-probes-multi.mjs output. No cgo, no new modules.
package bench

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// GPUSample is one nvidia-smi reading.
type GPUSample struct {
	EncoderPct int // utilization.encoder, 0..100
	VRAMMiB    int // memory.used, MiB
}

// NvidiaSmiArgs is the query, as the stage specifies (-l 1 streams one line per second).
var NvidiaSmiArgs = []string{"--query-gpu=utilization.encoder,memory.used", "--format=csv,noheader,nounits", "-l", "1"}

// ParseNvidiaLine parses one "<utilization.encoder>, <memory.used>" line.
// A utilisation outside 0..100 is rejected, so a line with the two fields
// swapped (or any garbage such as "[N/A]") is an error rather than a number.
func ParseNvidiaLine(line string) (GPUSample, error) {
	parts := strings.Split(strings.TrimSpace(line), ",")
	if len(parts) != 2 {
		return GPUSample{}, fmt.Errorf("nvidia-smi line %q: want 2 fields", line)
	}
	util, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return GPUSample{}, fmt.Errorf("nvidia-smi line %q: encoder utilisation: %w", line, err)
	}
	mem, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return GPUSample{}, fmt.Errorf("nvidia-smi line %q: memory used: %w", line, err)
	}
	if util < 0 || util > 100 || mem < 0 {
		return GPUSample{}, fmt.Errorf("nvidia-smi line %q: out of range (fields swapped?)", line)
	}
	return GPUSample{EncoderPct: util, VRAMMiB: mem}, nil
}

// ParseNvidiaSmi parses a block of output. Blank lines are ignored, bad lines
// are counted in skipped; it errors only when no line parsed.
func ParseNvidiaSmi(text string) (samples []GPUSample, skipped int, err error) {
	for _, l := range strings.Split(text, "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		s, e := ParseNvidiaLine(l)
		if e != nil {
			skipped++
			err = e
			continue
		}
		samples = append(samples, s)
	}
	if len(samples) > 0 {
		err = nil
	} else if err == nil {
		err = fmt.Errorf("nvidia-smi: no output")
	}
	return samples, skipped, err
}

// Proc is one process-list row.
type Proc struct {
	PID, PPID int
	Name      string // base name, lower case, no .exe
}

// ParseProcessList parses lines of "<pid> <ppid> <name or path>" (ps on Unix,
// a Get-CimInstance projection on Windows). Rows that do not parse are skipped.
func ParseProcessList(text string) []Proc {
	var out []Proc
	for _, l := range strings.Split(text, "\n") {
		f := strings.Fields(l)
		if len(f) < 3 {
			continue
		}
		pid, e1 := strconv.Atoi(f[0])
		ppid, e2 := strconv.Atoi(f[1])
		if e1 != nil || e2 != nil {
			continue
		}
		name := strings.Join(f[2:], " ")
		if i := strings.LastIndexAny(name, `/\`); i >= 0 {
			name = name[i+1:]
		}
		name = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".exe")
		out = append(out, Proc{PID: pid, PPID: ppid, Name: name})
	}
	return out
}

// CountEncoders returns how many ffmpeg processes are direct children of a
// running play-host (any play-host process other than self), and how many
// play-host processes were found.
func CountEncoders(procs []Proc, self int) (ffmpeg, hosts int) {
	isHost := map[int]bool{}
	for _, p := range procs {
		if p.Name == "play-host" && p.PID != self {
			isHost[p.PID] = true
		}
	}
	for _, p := range procs {
		if p.Name == "ffmpeg" && isHost[p.PPID] {
			ffmpeg++
		}
	}
	return ffmpeg, len(isHost)
}

// SlotSummary is one slot's input-to-photon summary from the driver output.
type SlotSummary struct {
	Slot       int
	Viewers    int
	N          int
	P50, P95   float64
	FromSample bool // true: pooled from raw samples; false: the viewer's own p50/p95
}

// DriverSummary is the parsed driver result.
type DriverSummary struct {
	Label  string
	Slots  []SlotSummary
	TTFFms []float64 // every viewer's ttff_ms, in viewer order (joiner first frame)
}

type driverJSON struct {
	Label   string `json:"label"`
	Viewers []struct {
		Role   string   `json:"role"`
		Slot   *int     `json:"slot"`
		P50    *float64 `json:"p50_ms"`
		P95    *float64 `json:"p95_ms"`
		N      int      `json:"n"`
		TTFF   *float64 `json:"ttff_ms"`
		Sample []struct {
			Ms   float64 `json:"ms"`
			Slot *int    `json:"slot"`
		} `json:"samples"`
	} `json:"viewers"`
}

// Pct is the driver's nearest-rank percentile (run-probes-multi.mjs `pct`).
func Pct(a []float64, p float64) float64 {
	s := append([]float64(nil), a...)
	sort.Float64s(s)
	i := int(math.Ceil(p*float64(len(s)))) - 1
	if i < 0 {
		i = 0
	}
	return s[i]
}

// ParseDriver reads run-probes-multi.mjs output and groups the players'
// probe samples by slot (the sample's own slot, else the viewer's). Spectators
// carry no slot and are left out of the slot table. A slot whose viewers kept
// no raw samples falls back to a single viewer's p50_ms/p95_ms.
func ParseDriver(b []byte) (DriverSummary, error) {
	var d driverJSON
	if err := json.Unmarshal(b, &d); err != nil {
		return DriverSummary{}, fmt.Errorf("driver json: %w", err)
	}
	if len(d.Viewers) == 0 {
		return DriverSummary{}, fmt.Errorf("driver json: no viewers[]")
	}
	out := DriverSummary{Label: d.Label}
	ms := map[int][]float64{}
	viewers := map[int]int{}
	fb := map[int]SlotSummary{}
	for _, v := range d.Viewers {
		if v.TTFF != nil {
			out.TTFFms = append(out.TTFFms, *v.TTFF)
		}
		if v.Role == "spectator" {
			continue
		}
		if v.Slot != nil {
			viewers[*v.Slot]++
			if v.P50 != nil && v.P95 != nil {
				fb[*v.Slot] = SlotSummary{Slot: *v.Slot, N: v.N, P50: *v.P50, P95: *v.P95}
			}
		}
		for _, s := range v.Sample {
			slot := s.Slot
			if slot == nil {
				slot = v.Slot
			}
			if slot == nil {
				continue
			}
			ms[*slot] = append(ms[*slot], s.Ms)
			if _, ok := viewers[*slot]; !ok {
				viewers[*slot] = 1
			}
		}
	}
	for slot, n := range viewers {
		if a := ms[slot]; len(a) > 0 {
			out.Slots = append(out.Slots, SlotSummary{Slot: slot, Viewers: n, N: len(a), P50: Pct(a, 0.5), P95: Pct(a, 0.95), FromSample: true})
		} else if f, ok := fb[slot]; ok {
			f.Viewers = n
			out.Slots = append(out.Slots, f)
		}
	}
	if len(out.Slots) == 0 {
		return out, fmt.Errorf("driver json: no player slot with samples or p50/p95")
	}
	sort.Slice(out.Slots, func(i, j int) bool { return out.Slots[i].Slot < out.Slots[j].Slot })
	return out, nil
}
