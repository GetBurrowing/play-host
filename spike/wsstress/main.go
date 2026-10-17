// Command wsstress drives the REAL internal/suite client against a live core
// runtime socket, to find where the play-host -> core websocket breaks
// (task 01a0dbd6 stage 6). It joins as its own seeded runtime (never the
// play host's), pushes an event core ignores (`s6_stress`, handled by
// RuntimeChannel's catch-all) with a payload size drawn from [min, max] bytes
// by `workers` goroutines at `rate` pushes per second in total, and
// heartbeats every `hb`. It reports every disconnect with its error, and
// exits non-zero if the socket ever dropped.
//
// Everything written to the socket goes through suite.Client.Push, so the
// run exercises exactly the write path the play host uses.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Startup-Suite/play-host/internal/suite"
)

type handler struct {
	connected chan struct{}
	drops     atomic.Int64
	events    atomic.Int64
	mu        sync.Mutex
	errs      []string
}

func (h *handler) OnEvent(string, json.RawMessage) { h.events.Add(1) }
func (h *handler) OnConnected() {
	select {
	case h.connected <- struct{}{}:
	default:
	}
}
func (h *handler) OnDisconnected(err error) {
	h.drops.Add(1)
	h.mu.Lock()
	h.errs = append(h.errs, fmt.Sprintf("%s %v", time.Now().UTC().Format("15:04:05.000"), err))
	h.mu.Unlock()
	log.Printf("DISCONNECTED: %v", err)
}

func main() {
	url := flag.String("url", "ws://127.0.0.1:4033/runtime/ws", "core runtime socket")
	rt := flag.String("runtime", "", "runtime_id (a stress runtime, never the play host's)")
	tok := flag.String("token-file", "", "runtime token file")
	dur := flag.Duration("dur", 2*time.Minute, "how long to push")
	rate := flag.Float64("rate", 40, "pushes per second, all workers together")
	workers := flag.Int("workers", 4, "concurrent pushing goroutines")
	minB := flag.Int("min", 1000, "smallest payload in bytes")
	maxB := flag.Int("max", 12000, "largest payload in bytes")
	hb := flag.Duration("hb", time.Second, "heartbeat interval")
	wbuf := flag.Int("write-buffer", -1, "Dialer.WriteBufferSize; -1 keeps the client's default")
	flag.Parse()
	// A play host's own runtime is named play-host-<host>; a stress runtime
	// must say so in its id, so a prod host's runtime is never borrowed.
	if *rt == "" || *tok == "" || (strings.HasPrefix(*rt, "play-host-") && !strings.Contains(*rt, "stress")) {
		log.Fatal("need -runtime (a stress runtime, not a play host's) and -token-file")
	}
	h := &handler{connected: make(chan struct{}, 1)}
	cfg := suite.Config{URL: *url, RuntimeID: *rt, TokenFile: *tok, Product: "wsstress", Heartbeat: *hb,
		Features: []string{"s6_stress"}}
	if *wbuf >= 0 {
		cfg.WriteBufferSize = *wbuf
	}
	cl := suite.New(cfg, h)
	ctx, cancel := context.WithCancel(context.Background())
	go cl.Run(ctx)
	select {
	case <-h.connected:
	case <-time.After(20 * time.Second):
		log.Fatal("never joined")
	}
	log.Printf("joined; pushing %d-%d B at %.0f/s with %d workers for %s, heartbeat %s, write buffer %d",
		*minB, *maxB, *rate, *workers, *dur, *hb, *wbuf)
	var pushed, failed, bytes atomic.Int64
	var seq atomic.Int64
	end := time.Now().Add(*dur)
	per := time.Duration(float64(time.Second) * float64(*workers) / *rate)
	var wg sync.WaitGroup
	for w := 0; w < *workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := time.NewTicker(per)
			defer t.Stop()
			for time.Now().Before(end) {
				<-t.C
				n := *minB + rand.IntN(*maxB-*minB+1)
				p := map[string]any{"seq": seq.Add(1), "pad": strings.Repeat("x", n)}
				if err := cl.Push("s6_stress", p); err != nil {
					failed.Add(1)
					continue
				}
				pushed.Add(1)
				bytes.Add(int64(n))
			}
		}()
	}
	wg.Wait()
	time.Sleep(3 * *hb) // one more heartbeat round trip must succeed
	cancel()
	h.mu.Lock()
	defer h.mu.Unlock()
	fmt.Printf("RESULT pushed=%d failed=%d bytes=%d drops=%d events=%d errs=%q\n",
		pushed.Load(), failed.Load(), bytes.Load(), h.drops.Load(), h.events.Load(), h.errs)
	if h.drops.Load() > 0 {
		os.Exit(1)
	}
}
