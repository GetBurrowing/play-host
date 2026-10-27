package media

import (
	"os"
	"strings"
	"testing"
)

// goldenGodotArgv is the ffmpeg argv of a Godot session as it was before
// capture Sources existed (ControlPreset, NVENC, 1280x720 addon frames).
const goldenGodotArgv = "-hide_banner -loglevel warning -nostats -y " +
	"-f rawvideo -pix_fmt rgba -video_size 1280x720 -framerate 60 -use_wallclock_as_timestamps 1 " +
	"-fflags nobuffer -flags low_delay -probesize 32 -i pipe:0 -fps_mode passthrough " +
	"-c:v h264_nvenc -preset p1 -tune ll -rc cbr -b:v 8000k -maxrate 8000k -bufsize 266k " +
	"-zerolatency 1 -delay 0 -rc-lookahead 0 -bf 0 -profile:v baseline -forced-idr 1 -g 120 " +
	"-bsf:v dump_extra=freq=keyframe " +
	"-an -flush_packets 1 -f rtp -payload_type 96 rtp://127.0.0.1:%d?pkt_size=1200"

func startArgv(t *testing.T, port int, src InputSource, w, h int) string {
	t.Helper()
	exe, _ := os.Executable()
	t.Setenv("FAKE_FFMPEG", "1")
	p := &Pipeline{FFmpeg: exe, Preset: ControlPreset(), Encoder: EncoderNVENC, RTPPort: port, Track: &sink{}, Source: src}
	if err := p.Start(w, h); err != nil {
		t.Fatal(err)
	}
	defer p.Stop()
	return strings.Join(p.Args, " ")
}

// A Godot session has no Source and must keep today's argv byte for byte.
func TestGodotPipelineArgvUnchanged(t *testing.T) {
	want := strings.Replace(goldenGodotArgv, "%d", "40393", 1)
	if got := startArgv(t, 40393, nil, 1280, 720); got != want {
		t.Fatalf("godot argv changed:\n got %s\nwant %s", got, want)
	}
}

type rawSource struct{}

func (rawSource) InputArgs(w, h, fps int) []string { return RawInputArgs("rgba", w, h, fps) }

type nativeSource struct{}

func (nativeSource) InputArgs(_, _, fps int) []string {
	return []string{"-f", "lavfi", "-i", "ddagrab=output_idx=1:framerate=60:draw_mouse=0:offset_x=100:offset_y=50:video_size=1280x720"}
}

// A pushing Source that returns RawInputArgs is the same argv; a native
// Source's input replaces it (so ignoring the Source would be caught here).
func TestPipelineArgvComesFromSource(t *testing.T) {
	want := strings.Replace(goldenGodotArgv, "%d", "40394", 1)
	if got := startArgv(t, 40394, rawSource{}, 1280, 720); got != want {
		t.Fatalf("raw source argv:\n got %s\nwant %s", got, want)
	}
	got := startArgv(t, 40395, nativeSource{}, 1280, 720)
	if !strings.Contains(got, "-f lavfi -i ddagrab=output_idx=1:framerate=60:draw_mouse=0:offset_x=100:offset_y=50:video_size=1280x720 ") ||
		strings.Contains(got, "rawvideo") || strings.Contains(got, "pipe:0") {
		t.Fatalf("native source argv:\n %s", got)
	}
}

type nvencOnly struct{ nativeSource }

func (nvencOnly) RequiredEncoder() EncoderKind { return EncoderNVENC }

// ddagrab needs NVENC: a libx264 session (or a libx264 fallback) must fail
// with a clear error instead of running a broken argv.
func TestNativeSourceRequiresNVENC(t *testing.T) {
	exe, _ := os.Executable()
	t.Setenv("FAKE_FFMPEG", "1")
	p := &Pipeline{FFmpeg: exe, Preset: ControlPreset(), Encoder: EncoderLibx264, RTPPort: 40396, Track: &sink{}, Source: nvencOnly{}}
	err := p.Start(1280, 720)
	if err == nil || !strings.Contains(err.Error(), "nvenc") {
		p.Stop()
		t.Fatalf("got %v", err)
	}
	if p.Running() {
		t.Fatal("pipeline started anyway")
	}
	ok := &Pipeline{FFmpeg: exe, Preset: ControlPreset(), Encoder: EncoderNVENC, RTPPort: 40397, Track: &sink{}, Source: nvencOnly{}}
	if err := ok.Start(1280, 720); err != nil {
		t.Fatal(err)
	}
	ok.Stop()
}
