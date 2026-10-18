// Command tcpcheck isolates the byte path between the play host (host-a) and
// core's dev container on host-b from any websocket or Phoenix code (task
// 01a0dbd6 stage 6). `serve` accepts TCP connections and checks every byte
// against a PRNG stream seeded by the connection's first 8 bytes; `send`
// writes that stream in writes of random size (like websocket frames) with
// short pauses, so a stale, dropped or duplicated chunk anywhere on the
// path shows up as a mismatch at an exact offset.
package main

import (
	"bufio"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

func stream(seed uint64) *rand.ChaCha8 {
	var s [32]byte
	binary.LittleEndian.PutUint64(s[:], seed)
	return rand.NewChaCha8(s)
}

func serve(addr string, slow time.Duration, readSize int, reply time.Duration, replyBytes int) {
	l, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("tcpcheck serving %s", addr)
	for {
		c, err := l.Accept()
		if err != nil {
			log.Fatal(err)
		}
		go func(c net.Conn) {
			defer c.Close()
			r := bufio.NewReaderSize(c, readSize)
			var hdr [8]byte
			if _, err := io.ReadFull(r, hdr[:]); err != nil {
				return
			}
			seed := binary.LittleEndian.Uint64(hdr[:])
			g := stream(seed)
			if reply > 0 {
				// Traffic the other way on the same connection, like
				// heartbeat replies and core's pushes to the host.
				stop := make(chan struct{})
				defer close(stop)
				go func() {
					t := time.NewTicker(reply)
					defer t.Stop()
					msg := make([]byte, replyBytes)
					for {
						select {
						case <-stop:
							return
						case <-t.C:
							if _, err := c.Write(msg); err != nil {
								return
							}
						}
					}
				}()
			}
			buf := make([]byte, readSize)
			want := make([]byte, readSize)
			var off int64
			for {
				if slow > 0 {
					// A slow reader (Bandit reads one frame at a time with
					// active: once) keeps the path's buffers full.
					time.Sleep(time.Duration(rand.Int64N(int64(slow))))
				}
				n, err := r.Read(buf)
				if n > 0 {
					g.Read(want[:n])
					for i := 0; i < n; i++ {
						if buf[i] != want[i] {
							log.Printf("MISMATCH conn %s seed %d at byte %d (read of %d bytes, index %d): got % x want % x",
								c.RemoteAddr(), seed, off+int64(i), n, i, buf[i:min(n, i+16)], want[i:min(n, i+16)])
							fmt.Fprintf(c, "MISMATCH %d\n", off+int64(i))
							return
						}
					}
					off += int64(n)
				}
				if err != nil {
					log.Printf("conn %s seed %d: %d bytes verified clean (%v)", c.RemoteAddr(), seed, off, err)
					return
				}
			}
		}(c)
	}
}

func send(addr string, total int64, minW, maxW, conns int, pause time.Duration) {
	var wg sync.WaitGroup
	var sent atomic.Int64
	for k := 0; k < conns; k++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			c, err := net.Dial("tcp", addr)
			if err != nil {
				log.Printf("dial: %v", err)
				return
			}
			defer c.Close()
			seed := uint64(time.Now().UnixNano()) + uint64(k)
			var hdr [8]byte
			binary.LittleEndian.PutUint64(hdr[:], seed)
			c.Write(hdr[:])
			g := stream(seed)
			drained := make(chan struct{})
			go func() { // discard what the server sends back, until MISMATCH or EOF
				defer close(drained)
				rb := make([]byte, 1<<16)
				for {
					n, err := c.Read(rb)
					if n > 0 && string(rb[:min(n, 8)]) == "MISMATCH" {
						log.Printf("conn %d seed %d: server says %s", k, seed, rb[:n])
					}
					if err != nil {
						return
					}
				}
			}()
			buf := make([]byte, maxW)
			var off int64
			for off < total {
				n := minW + rand.IntN(maxW-minW+1)
				g.Read(buf[:n])
				if _, err := c.Write(buf[:n]); err != nil {
					log.Printf("conn %d: write at %d: %v", k, off, err)
					return
				}
				off += int64(n)
				sent.Add(int64(n))
				if pause > 0 && rand.IntN(4) == 0 {
					time.Sleep(pause)
				}
			}
			c.(*net.TCPConn).CloseWrite()
			<-drained
		}(k)
	}
	wg.Wait()
	log.Printf("sent %d bytes on %d connection(s)", sent.Load(), conns)
}

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: tcpcheck serve -addr :9000 | send -addr host:port")
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	addr := fs.String("addr", ":9000", "listen or dial address")
	total := fs.Int64("bytes", 200<<20, "bytes per connection")
	minW := fs.Int("min", 1000, "smallest write")
	maxW := fs.Int("max", 16000, "largest write")
	conns := fs.Int("conns", 4, "parallel connections")
	pause := fs.Duration("pause", time.Millisecond, "pause after about 1 in 4 writes")
	slow := fs.Duration("slow", 0, "serve: sleep up to this long before each read")
	readSize := fs.Int("read", 1<<16, "serve: read size")
	reply := fs.Duration("reply", 0, "serve: write replyBytes back this often")
	replyBytes := fs.Int("reply-bytes", 40, "serve: bytes per reply")
	fs.Parse(os.Args[2:])
	switch os.Args[1] {
	case "serve":
		serve(*addr, *slow, *readSize, *reply, *replyBytes)
	case "send":
		send(*addr, *total, *minW, *maxW, *conns, *pause)
	}
}
