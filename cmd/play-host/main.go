// Command play-host serves a Godot build to one browser over WebRTC.
//
//	play-host serve -config /path/to/config.json   (the Burrow play host)
//	play-host spike [flags]                        (the frame-path harness)
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"runtime"
	"time"

	"github.com/Startup-Suite/play-host/internal/launch"
	"github.com/Startup-Suite/play-host/internal/media"
	"github.com/Startup-Suite/play-host/internal/spike"
)

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "keynames" {
		if err := keynames(); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(os.Args) >= 2 && os.Args[1] == "serve" {
		if err := serve(os.Args[2:]); err != nil {
			log.Fatalf("serve: %v", err)
		}
		return
	}
	if len(os.Args) < 2 || os.Args[1] != "spike" {
		fmt.Fprintln(os.Stderr, "usage: play-host serve -config <config.json> | play-host spike [flags]")
		os.Exit(2)
	}
	fs := flag.NewFlagSet("spike", flag.ExitOnError)
	p := media.DefaultPreset()
	var o spike.Options
	var framing, encoder string
	var dur time.Duration
	fs.StringVar(&o.Source, "source", "C", "frame path: A (ddagrab) | B (gdigrab) | C (in-engine readback)")
	fs.StringVar(&o.Export, "export", "image", "path C readback: image | async")
	fs.StringVar(&framing, "framing", "rtp", "ffmpeg output: rtp | annexb")
	fs.StringVar(&encoder, "encoder", string(media.EncoderAuto), "H.264 encoder: auto | nvenc | videotoolbox | libx264")
	fs.StringVar(&p.Preset, "preset", p.Preset, "NVENC preset p1..p7")
	fs.StringVar(&p.Tune, "tune", p.Tune, "NVENC tune ll | ull")
	fs.IntVar(&p.BitrateKbps, "bitrate", p.BitrateKbps, "CBR kbps")
	fs.IntVar(&p.FPS, "fps", p.FPS, "frames per second")
	fs.IntVar(&p.Width, "width", p.Width, "width")
	fs.IntVar(&p.Height, "height", p.Height, "height")
	fs.IntVar(&p.GOPFrames, "gop", p.GOPFrames, "GOP length in frames")
	fs.BoolVar(&p.IntraRefresh, "intra-refresh", false, "use -intra-refresh 1")
	fs.StringVar(&o.Godot, "godot", "", "Godot executable or macOS .app bundle")
	fs.StringVar(&o.RenderingDriver, "rendering-driver", launch.DefaultRenderingDriver(runtime.GOOS), "Godot rendering driver")
	fs.StringVar(&o.Project, "project", "", "Godot project dir")
	fs.StringVar(&o.FFmpeg, "ffmpeg", "ffmpeg", "ffmpeg exe")
	fs.StringVar(&o.Session, "session", "spike-01a0db5f", "session id (the --burrow-play-session= marker)")
	fs.IntVar(&o.GodotPort, "godot-port", 40320, "127.0.0.1 TCP port the addon listens on")
	fs.IntVar(&o.RTPPort, "rtp-port", 40330, "127.0.0.1 UDP port for ffmpeg RTP")
	fs.StringVar(&o.HTTPAddr, "http", "127.0.0.1:18431", "signalling listen address")
	fs.StringVar(&o.HostIP, "host-ip", "", "only offer host candidates on this IP")
	udpMin := fs.Uint("udp-min", 40300, "WebRTC UDP range start")
	udpMax := fs.Uint("udp-max", 40309, "WebRTC UDP range end")
	fs.StringVar(&o.LogDir, "logs", ".", "log directory")
	fs.DurationVar(&dur, "duration", 10*time.Minute, "stop after")
	fs.BoolVar(&o.NoGodot, "no-godot", false, "do not launch Godot (A/B smoke)")
	_ = fs.Parse(os.Args[2:])
	o.Framing = media.Framing(framing)
	o.Encoder = selectEncoderForOS(media.EncoderKind(encoder), runtime.GOOS)
	o.Preset = p
	o.UDPMin, o.UDPMax = uint16(*udpMin), uint16(*udpMax)
	o.Duration = dur
	if err := spike.Run(o); err != nil {
		log.Fatalf("spike: %v", err)
	}
}
