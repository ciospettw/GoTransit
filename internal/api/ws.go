package api

// Minimal RFC 6455 WebSocket server side — handshake, text frames, ping/pong,
// close. ~150 lines instead of a dependency; exactly what /v1/track needs.

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// WSConn is an accepted WebSocket connection.
type WSConn struct {
	conn net.Conn
	br   *bufio.Reader
	wmu  sync.Mutex
}

// UpgradeWS performs the server handshake.
func UpgradeWS(w http.ResponseWriter, r *http.Request) (*WSConn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") ||
		!strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") {
		return nil, fmt.Errorf("not a websocket upgrade")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		return nil, fmt.Errorf("missing Sec-WebSocket-Key")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, fmt.Errorf("connection cannot be hijacked")
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	h := sha1.Sum([]byte(key + wsGUID))
	accept := base64.StdEncoding.EncodeToString(h[:])
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := conn.Write([]byte(resp)); err != nil {
		conn.Close()
		return nil, err
	}
	return &WSConn{conn: conn, br: rw.Reader}, nil
}

// SendText writes one text frame (server frames are unmasked).
func (c *WSConn) SendText(payload []byte) error {
	return c.send(0x1, payload)
}

// Ping sends a ping frame.
func (c *WSConn) Ping() error { return c.send(0x9, nil) }

func (c *WSConn) send(opcode byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	var hdr [10]byte
	hdr[0] = 0x80 | opcode // FIN + opcode
	n := 2
	switch {
	case len(payload) < 126:
		hdr[1] = byte(len(payload))
	case len(payload) < 1<<16:
		hdr[1] = 126
		binary.BigEndian.PutUint16(hdr[2:4], uint16(len(payload)))
		n = 4
	default:
		hdr[1] = 127
		binary.BigEndian.PutUint64(hdr[2:10], uint64(len(payload)))
		n = 10
	}
	if _, err := c.conn.Write(hdr[:n]); err != nil {
		return err
	}
	_, err := c.conn.Write(payload)
	return err
}

// ReadMessage reads the next text/binary message, transparently answering
// pings and unmasking. Returns io.EOF on close.
func (c *WSConn) ReadMessage(timeout time.Duration) ([]byte, error) {
	var msg []byte
	for {
		if timeout > 0 {
			c.conn.SetReadDeadline(time.Now().Add(timeout))
		}
		var h [2]byte
		if _, err := io.ReadFull(c.br, h[:]); err != nil {
			return nil, err
		}
		fin := h[0]&0x80 != 0
		opcode := h[0] & 0x0f
		masked := h[1]&0x80 != 0
		ln := uint64(h[1] & 0x7f)
		switch ln {
		case 126:
			var ext [2]byte
			if _, err := io.ReadFull(c.br, ext[:]); err != nil {
				return nil, err
			}
			ln = uint64(binary.BigEndian.Uint16(ext[:]))
		case 127:
			var ext [8]byte
			if _, err := io.ReadFull(c.br, ext[:]); err != nil {
				return nil, err
			}
			ln = binary.BigEndian.Uint64(ext[:])
		}
		if ln > 1<<20 {
			return nil, fmt.Errorf("ws: frame too large")
		}
		var mask [4]byte
		if masked {
			if _, err := io.ReadFull(c.br, mask[:]); err != nil {
				return nil, err
			}
		}
		payload := make([]byte, ln)
		if _, err := io.ReadFull(c.br, payload); err != nil {
			return nil, err
		}
		if masked {
			for i := range payload {
				payload[i] ^= mask[i%4]
			}
		}
		switch opcode {
		case 0x8: // close
			c.send(0x8, nil)
			return nil, io.EOF
		case 0x9: // ping → pong
			c.send(0xA, payload)
			continue
		case 0xA: // pong
			continue
		case 0x0, 0x1, 0x2:
			msg = append(msg, payload...)
			if fin {
				return msg, nil
			}
		default:
			return nil, fmt.Errorf("ws: unsupported opcode %d", opcode)
		}
	}
}

// Close sends a close frame and tears the socket down.
func (c *WSConn) Close() error {
	c.send(0x8, nil)
	return c.conn.Close()
}
