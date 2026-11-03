package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/Startup-Suite/play-host/internal/bench"
)

// benchCmd is `play-host bench`: sample the running host's cost once a second.
func benchCmd(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config.json of the host being measured (optional; printed in the header)")
	seconds := fs.Int("seconds", 60, "seconds to sample, one sample per second; 0 = one sample")
	driver := fs.String("driver", "", "results.json written by spike/driver/run-probes-multi.mjs (optional)")
	_ = fs.Parse(args)
	o := bench.Options{Seconds: *seconds, Driver: *driver}
	if *cfgPath != "" {
		c, err := LoadConfig(*cfgPath)
		if err != nil {
			return err
		}
		o.ConfigNote = fmt.Sprintf("config: encoder=%s per_slot=%t per_slot_bitrate_kbps=%d", c.Encoder, c.PerSlot, c.PerSlotBitrateKbps)
	}
	o.Out = os.Stdout
	return bench.Run(context.Background(), o)
}
