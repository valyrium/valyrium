// Package tunnel implements the reverse tunnel in
// docs/adr/0002-tunnel-relay.md: `valyrium relay` runs on a public host and
// terminates TLS, `valyrium tunnel` runs beside a gateway on a private
// network and dials out to it. Inbound public connections are piped, byte
// for byte, down that single outbound connection to the local gateway.
//
// Nothing here inspects what it carries. The gateway's own
// CLAUDE_GATEWAY_API_KEY is still the only thing gating API access; the
// token in this package authenticates which tunnel client the relay pipes
// to, and nothing more (ADR 0002 §5).
package tunnel

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// ALPNProto marks a control connection. A TLS client that negotiates it is
// a tunnel client; anything else is public traffic to be proxied (ADR 0002 §2).
const ALPNProto = "vtun/1"

// Frames are [4-byte length][1-byte type][4-byte stream-id][payload], all
// integers big-endian (ADR 0002 §1). length counts the payload alone; the
// 9-byte header is not included in it.
const frameHeaderSize = 9

// MaxPayloadSize bounds one frame's payload. ReadFrame rejects a larger
// declared length before allocating anything, so a peer cannot turn a
// four-byte header into an arbitrary allocation.
const MaxPayloadSize = 64 * 1024

// idleTimeout drops a control connection that has gone silent. The tunnel
// client pings well inside this window, so a dead link surfaces here rather
// than hanging until the OS gives up on the TCP connection (ADR 0002 §3).
const idleTimeout = 90 * time.Second

// writeTimeout bounds a single frame write so one wedged peer cannot pin the
// shared write path forever.
const writeTimeout = 30 * time.Second

// streamBuffer is how many frames may sit unread on one stream before the
// read loop stops to wait for that stream's consumer. Bounded deliberately:
// v1 has no per-stream flow control, so a stalled consumer applies
// backpressure to the whole connection instead of growing without limit.
const streamBuffer = 16

// acceptBuffer bounds streams the peer has opened but this side has not yet
// accepted.
const acceptBuffer = 64

type FrameType uint8

const (
	FrameOpen   FrameType = 1 // open a stream
	FrameData   FrameType = 2 // stream payload
	FrameClose  FrameType = 3 // stream is done; no DATA follows
	FramePing   FrameType = 4 // keepalive
	FramePong   FrameType = 5 // keepalive reply
	FrameAuth   FrameType = 6 // handshake only: payload is the bearer token
	FrameAuthOK FrameType = 7 // handshake only: the relay accepted the token
)

func (t FrameType) String() string {
	switch t {
	case FrameOpen:
		return "OPEN"
	case FrameData:
		return "DATA"
	case FrameClose:
		return "CLOSE"
	case FramePing:
		return "PING"
	case FramePong:
		return "PONG"
	case FrameAuth:
		return "AUTH"
	case FrameAuthOK:
		return "AUTH_OK"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", uint8(t))
	}
}

func (t FrameType) known() bool { return t >= FrameOpen && t <= FrameAuthOK }

// carriesStream reports whether t addresses a logical stream. The rest are
// connection-level and ride on stream 0.
func (t FrameType) carriesStream() bool {
	return t == FrameOpen || t == FrameData || t == FrameClose
}

type Frame struct {
	Type     FrameType
	StreamID uint32
	Payload  []byte
}

// validate enforces the invariants both ends rely on: a known type, a
// payload inside the frame limit, and a stream id consistent with the type.
func (f Frame) validate() error {
	if !f.Type.known() {
		return fmt.Errorf("tunnel: unknown frame type %d", uint8(f.Type))
	}
	if len(f.Payload) > MaxPayloadSize {
		return fmt.Errorf("tunnel: %s payload is %d bytes, over the %d-byte frame limit", f.Type, len(f.Payload), MaxPayloadSize)
	}
	if f.Type.carriesStream() && f.StreamID == 0 {
		return fmt.Errorf("tunnel: %s must name a stream, got stream 0", f.Type)
	}
	if !f.Type.carriesStream() && f.StreamID != 0 {
		return fmt.Errorf("tunnel: %s is connection-level, got stream %d", f.Type, f.StreamID)
	}
	return nil
}

func WriteFrame(w io.Writer, f Frame) error {
	if err := f.validate(); err != nil {
		return err
	}

	var hdr [frameHeaderSize]byte
	binary.BigEndian.PutUint32(hdr[0:4], uint32(len(f.Payload)))
	hdr[4] = byte(f.Type)
	binary.BigEndian.PutUint32(hdr[5:9], f.StreamID)

	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(f.Payload) == 0 {
		return nil
	}
	_, err := w.Write(f.Payload)
	return err
}

func ReadFrame(r io.Reader) (Frame, error) {
	var hdr [frameHeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Frame{}, err
	}

	length := binary.BigEndian.Uint32(hdr[0:4])
	if length > MaxPayloadSize {
		return Frame{}, fmt.Errorf("tunnel: frame declares a %d-byte payload, over the %d-byte limit", length, MaxPayloadSize)
	}

	f := Frame{
		Type:     FrameType(hdr[4]),
		StreamID: binary.BigEndian.Uint32(hdr[5:9]),
	}
	if length > 0 {
		f.Payload = make([]byte, length)
		if _, err := io.ReadFull(r, f.Payload); err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return Frame{}, err
		}
	}
	if err := f.validate(); err != nil {
		return Frame{}, err
	}
	return f, nil
}

// Side selects which stream-id parity this end allocates from, so both ends
// may open streams without ever colliding on an id.
type Side int

const (
	SideRelay  Side = iota // accepted the control connection: odd ids
	SideTunnel             // dialed the control connection: even ids
)

// Conn multiplexes logical streams over one underlying connection. Writes
// are serialized so frames never interleave, and a single read loop
// demultiplexes inbound frames onto per-stream buffered channels so one
// slow stream does not head-of-line-block the others until its own buffer
// fills.
type Conn struct {
	raw net.Conn
	br  *bufio.Reader

	wmu sync.Mutex // serializes frame writes; the only writer of bw
	bw  *bufio.Writer

	mu      sync.Mutex
	streams map[uint32]*Stream
	nextID  uint32

	accept chan *Stream

	closeOnce sync.Once
	closed    chan struct{}

	errMu sync.Mutex
	err   error
}

// NewConn multiplexes raw and starts reading from it. raw must be past any
// handshake: the handshake reads exact frames straight off the connection
// precisely so that no bytes are left buffered anywhere else by the time
// this reader takes over.
func NewConn(raw net.Conn, side Side) *Conn {
	first := uint32(1)
	if side == SideTunnel {
		first = 2
	}

	c := &Conn{
		raw:     raw,
		br:      bufio.NewReader(raw),
		bw:      bufio.NewWriter(raw),
		streams: make(map[uint32]*Stream),
		nextID:  first,
		accept:  make(chan *Stream, acceptBuffer),
		closed:  make(chan struct{}),
	}
	go c.readLoop()
	return c
}

// Open starts a stream toward the peer. The relay opens one per inbound
// public connection; the tunnel client never opens any.
func (c *Conn) Open() (*Stream, error) {
	select {
	case <-c.closed:
		return nil, c.Err()
	default:
	}

	c.mu.Lock()
	id := c.nextID
	c.nextID += 2
	s := newStream(c, id)
	c.streams[id] = s
	c.mu.Unlock()

	if err := c.writeFrame(Frame{Type: FrameOpen, StreamID: id}); err != nil {
		c.removeStream(id)
		return nil, err
	}
	return s, nil
}

// Accept returns the next stream the peer opened.
func (c *Conn) Accept() (*Stream, error) {
	select {
	case s := <-c.accept:
		return s, nil
	case <-c.closed:
		return nil, c.Err()
	}
}

// Keepalive pings the peer on an interval until the connection ends. Any
// inbound frame — the PONG included — refreshes the read deadline, so an
// interval well under idleTimeout is what holds an otherwise idle tunnel,
// and the NAT mapping in front of it, open (ADR 0002 §3).
func (c *Conn) Keepalive(interval time.Duration) {
	if interval <= 0 {
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := c.writeFrame(Frame{Type: FramePing}); err != nil {
				return
			}
		case <-c.closed:
			return
		}
	}
}

// Done is shut once the connection is finished; Err reports why.
func (c *Conn) Done() <-chan struct{} { return c.closed }

func (c *Conn) Err() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	if c.err == nil {
		return net.ErrClosed
	}
	return c.err
}

func (c *Conn) Close() error {
	c.closeWithErr(net.ErrClosed)
	return nil
}

func (c *Conn) closeWithErr(err error) {
	c.closeOnce.Do(func() {
		c.errMu.Lock()
		if c.err == nil {
			c.err = err
		}
		c.errMu.Unlock()

		close(c.closed)
		_ = c.raw.Close()
	})
}

func (c *Conn) writeFrame(f Frame) error {
	// A frame this side built wrong is a bug here, not a broken peer: report
	// it without tearing the connection down.
	if err := f.validate(); err != nil {
		return err
	}

	c.wmu.Lock()
	defer c.wmu.Unlock()

	select {
	case <-c.closed:
		return c.Err()
	default:
	}

	// Setting a deadline only fails on an already-closed connection, which the
	// write below reports anyway.
	_ = c.raw.SetWriteDeadline(time.Now().Add(writeTimeout))
	if err := WriteFrame(c.bw, f); err != nil {
		c.closeWithErr(err)
		return err
	}
	if err := c.bw.Flush(); err != nil {
		c.closeWithErr(err)
		return err
	}
	return nil
}

func (c *Conn) readLoop() {
	for {
		_ = c.raw.SetReadDeadline(time.Now().Add(idleTimeout))

		f, err := ReadFrame(c.br)
		if err != nil {
			c.closeWithErr(err)
			return
		}

		switch f.Type {
		case FrameOpen:
			c.handleOpen(f.StreamID)
		case FrameData:
			s := c.stream(f.StreamID)
			if s == nil {
				// The stream is already gone here; tell the peer to stop. The
				// notice is best-effort: if it cannot be written the connection
				// is already failing, and the next read reports that.
				_ = c.writeFrame(Frame{Type: FrameClose, StreamID: f.StreamID})
				continue
			}
			s.deliver(f.Payload)
		case FrameClose:
			if s := c.stream(f.StreamID); s != nil {
				s.remoteClosed()
				c.removeStream(f.StreamID)
			}
		case FramePing:
			_ = c.writeFrame(Frame{Type: FramePong})
		case FramePong:
			// Reading it already refreshed the idle deadline above, which is
			// the entire point of the exchange.
		case FrameAuth, FrameAuthOK:
			c.closeWithErr(fmt.Errorf("tunnel: unexpected %s frame after the handshake", f.Type))
			return
		}
	}
}

func (c *Conn) handleOpen(id uint32) {
	c.mu.Lock()
	if _, live := c.streams[id]; live {
		c.mu.Unlock()
		c.closeWithErr(fmt.Errorf("tunnel: peer reopened live stream %d", id))
		return
	}
	s := newStream(c, id)
	c.streams[id] = s
	c.mu.Unlock()

	select {
	case c.accept <- s:
	default:
		// Nothing on this side is accepting streams — the relay never expects
		// the tunnel client to open one. Refuse it rather than let a peer
		// wedge the read loop.
		c.removeStream(id)
		_ = c.writeFrame(Frame{Type: FrameClose, StreamID: id})
	}
}

func (c *Conn) stream(id uint32) *Stream {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.streams[id]
}

func (c *Conn) removeStream(id uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.streams, id)
}

// Stream is one logical connection over a Conn. It is an io.ReadWriteCloser
// so both ends can hand it straight to io.Copy.
type Stream struct {
	id   uint32
	conn *Conn

	reads chan []byte
	rbuf  []byte

	remoteOnce sync.Once // guards close(reads); only the read loop calls it

	closeOnce sync.Once
	closed    chan struct{}
}

func newStream(c *Conn, id uint32) *Stream {
	return &Stream{
		id:     id,
		conn:   c,
		reads:  make(chan []byte, streamBuffer),
		closed: make(chan struct{}),
	}
}

func (s *Stream) ID() uint32 { return s.id }

func (s *Stream) Read(p []byte) (int, error) {
	for {
		if len(s.rbuf) > 0 {
			n := copy(p, s.rbuf)
			s.rbuf = s.rbuf[n:]
			return n, nil
		}

		select {
		case b, ok := <-s.reads:
			if !ok {
				return 0, io.EOF
			}
			s.rbuf = b
		case <-s.closed:
			return 0, io.EOF
		case <-s.conn.closed:
			return 0, s.conn.Err()
		}
	}
}

func (s *Stream) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		select {
		case <-s.closed:
			return written, net.ErrClosed
		case <-s.conn.closed:
			return written, s.conn.Err()
		default:
		}

		chunk := p
		if len(chunk) > MaxPayloadSize {
			chunk = chunk[:MaxPayloadSize]
		}
		if err := s.conn.writeFrame(Frame{Type: FrameData, StreamID: s.id, Payload: chunk}); err != nil {
			return written, err
		}

		written += len(chunk)
		p = p[len(chunk):]
	}
	return written, nil
}

// Close ends the stream and tells the peer. It is safe to call repeatedly
// and from several goroutines.
func (s *Stream) Close() error {
	var err error
	s.closeOnce.Do(func() {
		close(s.closed)
		s.conn.removeStream(s.id)
		err = s.conn.writeFrame(Frame{Type: FrameClose, StreamID: s.id})
	})
	return err
}

// deliver hands a DATA payload to the reader. It blocks while the stream's
// buffer is full: with no per-stream flow control in v1, bounded memory is
// worth more than isolation, so a stalled consumer backs pressure up the
// shared connection rather than letting it grow without limit.
func (s *Stream) deliver(b []byte) {
	if len(b) == 0 {
		return
	}
	select {
	case s.reads <- b:
	case <-s.closed:
	case <-s.conn.closed:
	}
}

// remoteClosed lets Read drain what is buffered and then report EOF. Only
// the connection's read loop calls it — the same goroutine that sends on
// reads — so closing the channel here cannot race with a send.
func (s *Stream) remoteClosed() {
	s.remoteOnce.Do(func() { close(s.reads) })
}

// pipe copies bytes both ways until either end finishes, then closes both.
// The first EOF means one end hung up, and every byte it sent has already
// been written to the other end by the copy that just returned — the relay
// is an L4 pipe and never parses what it carries (ADR 0002 §2), so this is
// all "done" can mean here.
func pipe(a, b io.ReadWriteCloser) {
	done := make(chan struct{}, 2)
	copyOne := func(dst io.Writer, src io.Reader) {
		// A copy error is one of the ends hanging up, which is the signal to
		// tear the pipe down rather than something to report.
		_, _ = io.Copy(dst, src)
		done <- struct{}{}
	}

	go copyOne(a, b)
	go copyOne(b, a)

	<-done
	_ = a.Close()
	_ = b.Close()
	<-done
}
