package bench

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// Options configures Run.
type Options struct {
	Seconds    int    // samples taken: Seconds, one per second; 0 = one sample now
	Driver     string // optional results.json from run-probes-multi.mjs
	ConfigNote string // one header line describing the config, may be empty
	Out        io.Writer
	// Seams for tests; nil = the real thing.
	ListProcs func() (string, error)
	StartGPU  func(ctx context.Context) (lines <-chan string, na string)
	Tick      time.Duration // default 1s
}

// ListProcsOS runs the platform's process listing as "<pid> <ppid> <name>".
func ListProcsOS() (string, error) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("powershell", "-NoProfile", "-Command",
			`Get-CimInstance Win32_Process | ForEach-Object { "$($_.ProcessId) $($_.ParentProcessId) $($_.Name)" }`)
	} else {
		cmd = exec.Command("ps", "-Ao", "pid=,ppid=,comm=")
	}
	b, err := cmd.Output()
	return string(b), err
}

// StartNvidiaSmi streams nvidia-smi lines; when it cannot start it returns
// a nil channel and the reason.
func StartNvidiaSmi(ctx context.Context) (<-chan string, string) {
	path, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return nil, "nvidia-smi not found on PATH"
	}
	cmd := exec.CommandContext(ctx, path, NvidiaSmiArgs...)
	so, err := cmd.StdoutPipe()
	if err != nil {
		return nil, "nvidia-smi: " + err.Error()
	}
	if err := cmd.Start(); err != nil {
		return nil, "nvidia-smi: " + err.Error()
	}
	ch := make(chan string, 64)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(so)
		for sc.Scan() {
			select {
			case ch <- sc.Text():
			default: // reader is behind; the next tick takes the freshest anyway
			}
		}
		_ = cmd.Wait()
	}()
	return ch, ""
}

type row struct {
	t       int
	enc     int
	encOK   bool
	gpu     GPUSample
	gpuOK   bool
	gpuNote string
}

// Run samples once a second and prints one table.
func Run(ctx context.Context, o Options) error {
	w := o.Out
	if w == nil {
		w = os.Stdout
	}
	list := o.ListProcs
	if list == nil {
		list = ListProcsOS
	}
	start := o.StartGPU
	if start == nil {
		start = StartNvidiaSmi
	}
	tick := o.Tick
	if tick == 0 {
		tick = time.Second
	}
	gctx, cancel := context.WithCancel(ctx)
	defer cancel()
	lines, na := start(gctx)

	var mu sync.Mutex
	var last *GPUSample
	var bad string
	if lines != nil {
		go func() {
			for l := range lines {
				s, err := ParseNvidiaLine(l)
				mu.Lock()
				if err != nil {
					bad = err.Error()
				} else {
					last, bad = &s, ""
				}
				mu.Unlock()
			}
		}()
	}

	var rows []row
	noHost := false
	n := o.Seconds
	if n < 1 {
		n = 1
	}
	for i := 0; i < n; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				i = n
				continue
			case <-time.After(tick):
			}
		} else if lines != nil {
			// the first sample waits briefly for nvidia-smi's first line
			for w := 0; w < 30; w++ {
				mu.Lock()
				ok := last != nil
				mu.Unlock()
				if ok {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
		}
		r := row{t: i}
		if txt, err := list(); err == nil {
			ff, hosts := CountEncoders(ParseProcessList(txt), os.Getpid())
			r.enc, r.encOK = ff, hosts > 0
			if hosts == 0 {
				noHost = true
			}
		}
		mu.Lock()
		switch {
		case lines == nil:
			r.gpuNote = na
		case last != nil:
			r.gpu, r.gpuOK = *last, true
		case bad != "":
			r.gpuNote = "unparsable: " + bad
		default:
			r.gpuNote = "no nvidia-smi output yet"
		}
		mu.Unlock()
		rows = append(rows, r)
	}
	cancel()

	if o.ConfigNote != "" {
		fmt.Fprintln(w, o.ConfigNote)
	}
	fmt.Fprintf(w, "%-5s %-9s %-8s %-9s\n", "t(s)", "encoders", "NVENC %", "VRAM MiB")
	var encs, nv, vr []float64
	var reason string
	for _, r := range rows {
		e, nvs, vs := "n/a", "n/a", "n/a"
		if r.encOK {
			e = fmt.Sprint(r.enc)
			encs = append(encs, float64(r.enc))
		}
		if r.gpuOK {
			nvs, vs = fmt.Sprint(r.gpu.EncoderPct), fmt.Sprint(r.gpu.VRAMMiB)
			nv = append(nv, float64(r.gpu.EncoderPct))
			vr = append(vr, float64(r.gpu.VRAMMiB))
		} else if reason == "" {
			reason = r.gpuNote
		}
		fmt.Fprintf(w, "%-5d %-9s %-8s %-9s\n", r.t, e, nvs, vs)
	}
	fmt.Fprintf(w, "summary: encoders %s | NVENC %% %s | VRAM MiB %s\n", mmm(encs), mmm(nv), mmm(vr))
	if noHost {
		fmt.Fprintln(w, "encoders: n/a where no running play-host process was found (start `play-host serve` and a session first)")
	}
	if reason != "" {
		fmt.Fprintf(w, "NVENC / VRAM: n/a (%s)\n", reason)
	}

	if o.Driver != "" {
		b, err := os.ReadFile(o.Driver)
		if err != nil {
			return err
		}
		d, err := ParseDriver(b)
		if err != nil {
			return fmt.Errorf("%s: %w", o.Driver, err)
		}
		fmt.Fprintf(w, "\ndriver %q: per-slot input-to-photon\n%-5s %-8s %-5s %-8s %-8s\n", d.Label, "slot", "viewers", "n", "p50 ms", "p95 ms")
		for _, s := range d.Slots {
			src := ""
			if !s.FromSample {
				src = " (viewer's own p50/p95, no raw samples)"
			}
			fmt.Fprintf(w, "%-5d %-8d %-5d %-8.1f %-8.1f%s\n", s.Slot, s.Viewers, s.N, s.P50, s.P95, src)
		}
		if len(d.TTFFms) > 0 {
			ts := make([]string, len(d.TTFFms))
			for i, t := range d.TTFFms {
				ts[i] = fmt.Sprintf("%.0f", t)
			}
			fmt.Fprintf(w, "joiner first frame (ttff_ms, viewer order): %s\n", strings.Join(ts, ", "))
		}
	}
	return nil
}

func mmm(a []float64) string {
	if len(a) == 0 {
		return "n/a"
	}
	s := append([]float64(nil), a...)
	sort.Float64s(s)
	return fmt.Sprintf("min %.0f median %.0f max %.0f", s[0], s[len(s)/2], s[len(s)-1])
}
