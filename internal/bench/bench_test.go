package bench

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseNvidiaLine(t *testing.T) {
	s, err := ParseNvidiaLine("12, 811")
	if err != nil || s.EncoderPct != 12 || s.VRAMMiB != 811 {
		t.Fatalf("got %+v %v", s, err)
	}
	if s, err = ParseNvidiaLine(" 0,551\r"); err != nil || s.EncoderPct != 0 || s.VRAMMiB != 551 {
		t.Fatalf("got %+v %v", s, err)
	}
}

func TestParseNvidiaLineRejects(t *testing.T) {
	for _, l := range []string{"811, 12", "", "12", "12, 811, 3", "[N/A], 811", "12, [N/A]", "abc, def", "-1, 5", "101, 5"} {
		if _, err := ParseNvidiaLine(l); err == nil {
			t.Errorf("%q parsed", l)
		}
	}
}

func TestParseNvidiaSmiBlock(t *testing.T) {
	got, skipped, err := ParseNvidiaSmi("4, 811\nNVIDIA-SMI has failed\n\n15, 1070\n")
	if err != nil || len(got) != 2 || skipped != 1 || got[1].VRAMMiB != 1070 {
		t.Fatalf("got %+v skipped=%d err=%v", got, skipped, err)
	}
	if _, _, err := ParseNvidiaSmi("garbage\nmore garbage\n"); err == nil {
		t.Fatal("all-garbage block must error")
	}
	if _, _, err := ParseNvidiaSmi(""); err == nil {
		t.Fatal("empty block must error")
	}
}

const ps = `    1     0 /sbin/launchd
  100     1 /opt/play-host/play-host
  101   100 /usr/bin/ffmpeg
  102   100 /usr/bin/ffmpeg
  103   100 /opt/godot/Godot
  200     1 /usr/bin/ffmpeg
  300     1 play-host
  nonsense
`

func TestCountEncoders(t *testing.T) {
	ff, hosts := CountEncoders(ParseProcessList(ps), 300) // 300 is the bench itself
	if ff != 2 || hosts != 1 {
		t.Fatalf("ffmpeg=%d hosts=%d", ff, hosts)
	}
	win := ParseProcessList("10 1 play-host.exe\r\n11 10 ffmpeg.exe\r\n12 10 Godot.exe\r\n")
	if ff, hosts := CountEncoders(win, 1); ff != 1 || hosts != 1 {
		t.Fatalf("windows ffmpeg=%d hosts=%d", ff, hosts)
	}
	if ff, hosts := CountEncoders(nil, 1); ff != 0 || hosts != 0 {
		t.Fatal("empty")
	}
}

const driverFixture = `{"label":"p2s1","viewers":[
 {"role":"player","slot":0,"p50_ms":99,"p95_ms":99,"n":4,"ttff_ms":1100,
  "samples":[{"ms":40,"slot":0},{"ms":42,"slot":0},{"ms":44,"slot":0},{"ms":60,"slot":0}]},
 {"role":"player","slot":1,"p50_ms":99,"p95_ms":99,"n":2,"ttff_ms":1300,
  "samples":[{"ms":50,"slot":1},{"ms":70,"slot":1}]},
 {"role":"spectator","slot":null,"n":0,"ttff_ms":1500,"samples":[]}]}`

func TestParseDriverPerSlot(t *testing.T) {
	d, err := ParseDriver([]byte(driverFixture))
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Slots) != 2 {
		t.Fatalf("slots %+v", d.Slots)
	}
	s0, s1 := d.Slots[0], d.Slots[1]
	if s0.Slot != 0 || s0.N != 4 || s0.P50 != 42 || s0.P95 != 60 || !s0.FromSample {
		t.Errorf("slot0 %+v", s0)
	}
	if s1.Slot != 1 || s1.N != 2 || s1.P50 != 50 || s1.P95 != 70 {
		t.Errorf("slot1 %+v", s1)
	}
	if len(d.TTFFms) != 3 || d.TTFFms[2] != 1500 {
		t.Errorf("ttff %v", d.TTFFms)
	}
}

func TestParseDriverFallbackAndErrors(t *testing.T) {
	d, err := ParseDriver([]byte(`{"viewers":[{"role":"player","slot":2,"p50_ms":41.5,"p95_ms":60.2,"n":50}]}`))
	if err != nil || len(d.Slots) != 1 || d.Slots[0].Slot != 2 || d.Slots[0].P95 != 60.2 || d.Slots[0].FromSample {
		t.Fatalf("%+v %v", d, err)
	}
	for _, bad := range []string{"not json", `{}`, `{"viewers":[{"role":"spectator"}]}`} {
		if _, err := ParseDriver([]byte(bad)); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestRunZeroSecondsOneSampleNoGPU(t *testing.T) {
	var out bytes.Buffer
	err := Run(context.Background(), Options{Seconds: 0, Out: &out,
		ListProcs: func() (string, error) { return ps, nil },
		StartGPU:  func(context.Context) (<-chan string, string) { return nil, "nvidia-smi not found on PATH" },
		Tick:      time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if strings.Count(s, "\n0 ")+strings.Count(s, "\n0    ") < 1 || strings.Contains(s, "\n1 ") {
		t.Errorf("want exactly one sample row:\n%s", s)
	}
	if !strings.Contains(s, "NVENC / VRAM: n/a (nvidia-smi not found on PATH)") {
		t.Errorf("missing n/a reason:\n%s", s)
	}
}

func TestRunWithGPUAndDriver(t *testing.T) {
	ch := make(chan string, 4)
	ch <- "garbage"
	ch <- "9, 811"
	var out bytes.Buffer
	dir := t.TempDir()
	f := dir + "/r.json"
	if err := writeFile(f, driverFixture); err != nil {
		t.Fatal(err)
	}
	err := Run(context.Background(), Options{Seconds: 1, Out: &out, Driver: f, ConfigNote: "config: x",
		ListProcs: func() (string, error) { return ps, nil },
		StartGPU:  func(context.Context) (<-chan string, string) { return ch, "" },
		Tick:      time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{"config: x", "9", "811", "driver \"p2s1\"", "joiner first frame"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q:\n%s", want, s)
		}
	}
}

func writeFile(p, s string) error { return os.WriteFile(p, []byte(s), 0o600) }
