package wakeproxy

import (
	"io"
	"net"
	"sync"
)

type closeWriter interface{ CloseWrite() error }

// splice copies both directions until each side is done. When one side
// finishes reading it half-closes the other, so a request can end while the
// response is still flowing. It returns the bytes copied a to b and b to a.
func splice(a, b net.Conn) (ab, ba int64) {
	var wg sync.WaitGroup

	cp := func(dst, src net.Conn, n *int64) {
		defer wg.Done()

		*n, _ = io.Copy(dst, src)

		if cw, ok := dst.(closeWriter); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
	}

	wg.Add(2)

	go cp(b, a, &ab)
	go cp(a, b, &ba)

	wg.Wait()

	return ab, ba
}

// replayConn is a client connection whose first byte has already been read:
// Read returns that byte first.
type replayConn struct {
	net.Conn
	r io.Reader
}

func (c *replayConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func (c *replayConn) CloseWrite() error {
	if cw, ok := c.Conn.(closeWriter); ok {
		return cw.CloseWrite()
	}

	return c.Conn.Close()
}
