package supported

import (
	"encoding/binary"
	"io"
	"net"
	"testing"
	"testing/synctest"
)

// This file shows how to make network logic testable under weave by faking the
// transport with an in-memory, bubble-aware net.Conn, and is EXPECTED TO PASS in
// both modes. Real sockets leave the synctest bubble (weave cannot control or
// explore them), so — as with loom/Coyote/CHESS — the transport must be built from
// in-memory seams (channels / sync primitives) that weave treats as scheduling
// points.
//
// Use net.Pipe as the fake transport. It is already bubble-aware (implemented with
// channels) and, crucially, COMPACT: its Read/Write is a synchronous rendezvous,
// one scheduling point per hand-off. That keeps the explored state space small.
//
// Avoid hand-rolling a *buffered* conn out of sync.Cond + a []byte buffer: under
// -weave every slice/flag access becomes a scheduling point, so the fake's own
// internals explode the search (a one-byte echo can exceed the schedule budget).
// If you truly need buffering, build it from a buffered channel (one scheduling
// point per message), not cond+slice. For most request/response and reconnect
// protocols, net.Pipe is enough.

// writeFrame / readFrame are a tiny length-prefixed message protocol, the kind
// of framing real network code uses. They run unchanged over the net.Pipe fake.
func writeFrame(c net.Conn, msg []byte) error {
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], uint16(len(msg)))
	if _, err := c.Write(hdr[:]); err != nil {
		return err
	}
	_, err := c.Write(msg)
	return err
}

func readFrame(c net.Conn) ([]byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		return nil, err
	}
	msg := make([]byte, binary.BigEndian.Uint16(hdr[:]))
	if _, err := io.ReadFull(c, msg); err != nil {
		return nil, err
	}
	return msg, nil
}

// TestFakeConnRequestResponse runs a length-prefixed request/response protocol
// over net.Pipe: a server goroutine reads one request frame and replies. weave
// explores every read/write hand-off across the connection. PASS.
func TestFakeConnRequestResponse(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cli, srv := net.Pipe()
		go func() {
			defer srv.Close()
			req, err := readFrame(srv)
			if err != nil {
				return
			}
			writeFrame(srv, append([]byte("echo:"), req...))
		}()

		if err := writeFrame(cli, []byte("ping")); err != nil {
			t.Fatalf("write: %v", err)
		}
		resp, err := readFrame(cli)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if string(resp) != "echo:ping" {
			t.Fatalf("got %q, want %q", resp, "echo:ping")
		}
		cli.Close()
	})
}
