package tunnel

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// TestMuxFrameRoundTrip pins the wire format in ADR 0002 §1 — both the codec
// on its own and the multiplexer built on top of it, since a frame that
// encodes and decodes but loses stream identity or ordering is no use to the
// byte pipe above it.
func TestMuxFrameRoundTrip(t *testing.T) {
	t.Run("codec preserves every frame", func(t *testing.T) {
		frames := []Frame{
			{Type: FrameOpen, StreamID: 1},
			{Type: FrameData, StreamID: 1, Payload: []byte("GET /v1/models HTTP/1.1\r\n\r\n")},
			{Type: FrameData, StreamID: 7, Payload: []byte{0x00, 0xff, 0x00}}, // binary, not text
			{Type: FrameData, StreamID: 2, Payload: bytes.Repeat([]byte("x"), MaxPayloadSize)},
			{Type: FrameClose, StreamID: 1},
			{Type: FramePing},
			{Type: FramePong},
			{Type: FrameAuth, Payload: []byte("s3cret")},
			{Type: FrameAuthOK},
		}

		// One buffer for all of them: frames have to stay aligned back to back,
		// which is the property the length prefix exists to provide.
		var wire bytes.Buffer
		for _, f := range frames {
			if err := WriteFrame(&wire, f); err != nil {
				t.Fatalf("WriteFrame(%s): %v", f.Type, err)
			}
		}

		for _, want := range frames {
			got, err := ReadFrame(&wire)
			if err != nil {
				t.Fatalf("ReadFrame: %v", err)
			}
			if got.Type != want.Type {
				t.Errorf("type: got %s, want %s", got.Type, want.Type)
			}
			if got.StreamID != want.StreamID {
				t.Errorf("%s stream id: got %d, want %d", want.Type, got.StreamID, want.StreamID)
			}
			if !bytes.Equal(got.Payload, want.Payload) {
				t.Errorf("%s payload: got %d bytes, want %d", want.Type, len(got.Payload), len(want.Payload))
			}
		}

		if wire.Len() != 0 {
			t.Errorf("%d bytes left over: frames are not tightly packed", wire.Len())
		}
		if _, err := ReadFrame(&wire); !errors.Is(err, io.EOF) {
			t.Errorf("reading a drained buffer should report io.EOF, got %v", err)
		}
	})

	t.Run("streams carry bytes both ways", func(t *testing.T) {
		relay, tunnel := muxPair(t)

		// The tunnel end reads a length-prefixed request and echoes it back
		// uppercased, then closes. There is no half-close in this protocol,
		// so the responder is what decides a stream is finished — which is
		// exactly how the gateway behaves at the far end of a real pipe.
		go func() {
			stream, err := tunnel.Accept()
			if err != nil {
				return
			}
			defer func() { _ = stream.Close() }()

			var size [4]byte
			if _, err := io.ReadFull(stream, size[:]); err != nil {
				return
			}
			body := make([]byte, binary.BigEndian.Uint32(size[:]))
			if _, err := io.ReadFull(stream, body); err != nil {
				return
			}
			_, _ = stream.Write([]byte(strings.ToUpper(string(body))))
		}()

		stream, err := relay.Open()
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer func() { _ = stream.Close() }()

		// Larger than one frame, so the round trip has to survive being split
		// across frames and reassembled in order.
		sent := bytes.Repeat([]byte("valyrium"), MaxPayloadSize/4)
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(sent)))

		go func() {
			_, _ = stream.Write(size[:])
			_, _ = stream.Write(sent)
		}()

		got, err := io.ReadAll(stream)
		if err != nil {
			t.Fatalf("reading the echo: %v", err)
		}
		if want := bytes.ToUpper(sent); !bytes.Equal(got, want) {
			t.Errorf("echo mismatch: got %d bytes, want %d", len(got), len(want))
		}
	})

	t.Run("concurrent streams stay separate", func(t *testing.T) {
		relay, tunnel := muxPair(t)

		go func() {
			for {
				stream, err := tunnel.Accept()
				if err != nil {
					return
				}
				// Reply with the stream's own id, so a crossed wire shows up as
				// the wrong answer rather than as silence.
				go func(s *Stream) {
					defer func() { _ = s.Close() }()
					body := make([]byte, 4)
					if _, err := io.ReadFull(s, body); err != nil {
						return
					}
					_, _ = s.Write(body)
				}(stream)
			}
		}()

		const streams = 8
		type reply struct {
			id  uint32
			got uint32
		}
		replies := make(chan reply, streams)

		for i := 0; i < streams; i++ {
			go func() {
				stream, err := relay.Open()
				if err != nil {
					replies <- reply{}
					return
				}
				defer func() { _ = stream.Close() }()

				var tag [4]byte
				binary.BigEndian.PutUint32(tag[:], stream.ID())
				if _, err := stream.Write(tag[:]); err != nil {
					replies <- reply{id: stream.ID()}
					return
				}

				var echoed [4]byte
				if _, err := io.ReadFull(stream, echoed[:]); err != nil {
					replies <- reply{id: stream.ID()}
					return
				}
				replies <- reply{id: stream.ID(), got: binary.BigEndian.Uint32(echoed[:])}
			}()
		}

		for i := 0; i < streams; i++ {
			select {
			case r := <-replies:
				if r.id != r.got {
					t.Errorf("stream %d got stream %d's bytes back", r.id, r.got)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for concurrent streams to answer")
			}
		}
	})
}

// TestMuxFrameMalformedRejected covers what a peer can put on the wire that
// this side must refuse. The allocation guard matters most: a four-byte
// length prefix is the cheapest way to ask a process to allocate more than it
// has (ADR 0002 §1).
func TestMuxFrameMalformedRejected(t *testing.T) {
	t.Run("reads", func(t *testing.T) {
		tests := []struct {
			name    string
			wire    []byte
			wantErr string
		}{
			{
				name: "empty input",
				wire: nil,
			},
			{
				name: "header cut short",
				wire: []byte{0x00, 0x00, 0x00},
			},
			{
				name:    "unknown frame type",
				wire:    header(0, 99, 0),
				wantErr: "unknown frame type 99",
			},
			{
				name:    "type zero",
				wire:    header(0, 0, 0),
				wantErr: "unknown frame type 0",
			},
			{
				name:    "declared payload over the frame limit",
				wire:    header(MaxPayloadSize+1, byte(FrameData), 1),
				wantErr: "over the 65536-byte limit",
			},
			{
				name:    "declared payload of 4GiB",
				wire:    header(^uint32(0), byte(FrameData), 1),
				wantErr: "over the 65536-byte limit",
			},
			{
				name: "payload shorter than the header claims",
				wire: append(header(64, byte(FrameData), 1), []byte("only a few bytes")...),
			},
			{
				name:    "stream frame on stream 0",
				wire:    append(header(3, byte(FrameData), 0), []byte("abc")...),
				wantErr: "must name a stream",
			},
			{
				name:    "connection frame on a stream",
				wire:    header(0, byte(FramePing), 9),
				wantErr: "connection-level",
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				_, err := ReadFrame(bytes.NewReader(tc.wire))
				if err == nil {
					t.Fatal("expected an error, got none")
				}
				if tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q does not mention %q", err, tc.wantErr)
				}
			})
		}
	})

	t.Run("an oversized length header allocates nothing", func(t *testing.T) {
		// The reader hands over a 4GiB length and then stops. If ReadFrame
		// trusted the header it would try to allocate 4GiB before discovering
		// there is no payload behind it.
		r := &countingReader{Reader: bytes.NewReader(header(^uint32(0), byte(FrameData), 1))}

		if _, err := ReadFrame(r); err == nil {
			t.Fatal("expected an oversized frame to be rejected")
		}
		if r.n > frameHeaderSize {
			t.Errorf("read %d bytes past the header before rejecting the frame", r.n-frameHeaderSize)
		}
	})

	t.Run("writes", func(t *testing.T) {
		tests := []struct {
			name    string
			frame   Frame
			wantErr string
		}{
			{
				name:    "payload over the frame limit",
				frame:   Frame{Type: FrameData, StreamID: 1, Payload: make([]byte, MaxPayloadSize+1)},
				wantErr: "over the 65536-byte frame limit",
			},
			{
				name:    "unknown type",
				frame:   Frame{Type: FrameType(42), StreamID: 1},
				wantErr: "unknown frame type 42",
			},
			{
				name:    "stream frame with no stream",
				frame:   Frame{Type: FrameData, Payload: []byte("x")},
				wantErr: "must name a stream",
			},
			{
				name:    "connection frame with a stream",
				frame:   Frame{Type: FramePong, StreamID: 3},
				wantErr: "connection-level",
			},
		}

		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				var wire bytes.Buffer
				err := WriteFrame(&wire, tc.frame)
				if err == nil {
					t.Fatal("expected an error, got none")
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error %q does not mention %q", err, tc.wantErr)
				}
				if wire.Len() != 0 {
					t.Errorf("a rejected frame put %d bytes on the wire", wire.Len())
				}
			})
		}
	})

	t.Run("a malformed frame kills the connection", func(t *testing.T) {
		// A peer that garbles the wire cannot be resynchronized — the length
		// prefix is the only thing that says where the next frame starts — so
		// the connection has to go down rather than carry on misreading it.
		local, remote := net.Pipe()
		mux := NewConn(local, SideRelay)
		t.Cleanup(func() { _ = mux.Close() })

		go func() {
			_, _ = remote.Write(header(8, 99, 1)) // unknown type
			_, _ = remote.Write([]byte("garbage!"))
		}()

		select {
		case <-mux.Done():
			if err := mux.Err(); !strings.Contains(err.Error(), "unknown frame type") {
				t.Errorf("connection died of %q, want the unknown frame type", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a malformed frame did not bring the connection down")
		}
	})
}

// header builds a frame header by hand, so the tests above depend on the wire
// format rather than on the encoder that produces it.
func header(length uint32, frameType byte, streamID uint32) []byte {
	b := make([]byte, frameHeaderSize)
	binary.BigEndian.PutUint32(b[0:4], length)
	b[4] = frameType
	binary.BigEndian.PutUint32(b[5:9], streamID)
	return b
}

type countingReader struct {
	io.Reader
	n int
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.n += n
	return n, err
}

// muxPair wires two multiplexers together over an in-memory connection, one
// on each side of the protocol.
func muxPair(t *testing.T) (relay, tunnel *Conn) {
	t.Helper()

	relayConn, tunnelConn := net.Pipe()
	relay = NewConn(relayConn, SideRelay)
	tunnel = NewConn(tunnelConn, SideTunnel)

	t.Cleanup(func() {
		_ = relay.Close()
		_ = tunnel.Close()
	})
	return relay, tunnel
}
