// Package link is the 127.0.0.1 TCP connection between the play host and the
// burrow_play addon inside Godot. Host -> game is newline-delimited JSON (see
// internal/input); game -> host multiplexes SPF1 video and SPA1 game PCM.
package link

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// Link is one addon connection.
type Link struct {
	conn net.Conn
	r    *bufio.Reader
	wmu  sync.Mutex
}

// Record is one multiplexed game -> host media record. Exactly one header is
// populated. Payload is valid until the next ReadRecord call reuses its
// buffer; consumers that queue it must copy it first.
type Record struct {
	Frame *FrameHeader
	PCM   *PCMHeader
	Data  []byte
}

// Dial connects to the addon, retrying until deadline (Godot takes a few
// seconds to boot and open its listener).
func Dial(port int, within time.Duration, alive func() bool) (*Link, error) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	end := time.Now().Add(within)
	for {
		c, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			if tc, ok := c.(*net.TCPConn); ok {
				_ = tc.SetNoDelay(true)
			}
			return New(c), nil
		}
		if time.Now().After(end) {
			return nil, fmt.Errorf("addon link %s: %w", addr, err)
		}
		if alive != nil && !alive() {
			return nil, fmt.Errorf("addon link %s: the game exited before listening", addr)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// New wraps an established connection.
func New(c net.Conn) *Link { return &Link{conn: c, r: bufio.NewReaderSize(c, 1<<20)} }

// Send writes one JSON line.
func (l *Link) Send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	l.wmu.Lock()
	defer l.wmu.Unlock()
	_ = l.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, err = l.conn.Write(b)
	return err
}

// ReadFrame reads one legacy video-only record into buf (grown as needed).
func (l *Link) ReadFrame(buf []byte) (FrameHeader, []byte, error) {
	h, err := ReadFrameHeader(l.r)
	if err != nil {
		return h, buf, err
	}
	if cap(buf) < int(h.Length) {
		buf = make([]byte, h.Length)
	}
	buf = buf[:h.Length]
	if _, err := io.ReadFull(l.r, buf); err != nil {
		return h, buf, err
	}
	return h, buf, nil
}

// ReadRecord reads one SPF1 video or SPA1 PCM record from the shared stream.
func (l *Link) ReadRecord(buf []byte) (Record, []byte, error) {
	magic, err := l.r.Peek(4)
	if err != nil {
		return Record{}, buf, err
	}
	var length uint32
	rec := Record{}
	switch [4]byte(magic) {
	case frameMagic:
		h, err := ReadFrameHeader(l.r)
		if err != nil {
			return Record{}, buf, err
		}
		rec.Frame, length = &h, h.Length
	case pcmMagic:
		h, err := ReadPCMHeader(l.r)
		if err != nil {
			return Record{}, buf, err
		}
		rec.PCM, length = &h, h.Length
	default:
		return Record{}, buf, fmt.Errorf("bad record magic %q", magic)
	}
	if cap(buf) < int(length) {
		buf = make([]byte, length)
	}
	buf = buf[:length]
	if _, err := io.ReadFull(l.r, buf); err != nil {
		return Record{}, buf, err
	}
	rec.Data = buf
	return rec, buf, nil
}

// Close closes the connection.
func (l *Link) Close() error { return l.conn.Close() }
