package erpc

import "net"

type dualStackListener struct {
	ln4, ln6 net.Listener
}

func (dl *dualStackListener) Accept() (net.Conn, error) {
	// Use a channel to communicate the result of Accept
	type result struct {
		conn net.Conn
		err  error
	}
	ch := make(chan result, 2)

	// Try to accept from both listeners
	go func() {
		conn, err := dl.ln4.Accept()
		ch <- result{conn, err}
	}()
	go func() {
		conn, err := dl.ln6.Accept()
		ch <- result{conn, err}
	}()

	// Return the first successful connection, or the last error
	var lastErr error
	for i := 0; i < 2; i++ {
		res := <-ch
		if res.err == nil {
			return res.conn, nil
		}
		lastErr = res.err
	}
	return nil, lastErr
}

func (dl *dualStackListener) Close() error {
	err4 := dl.ln4.Close()
	err6 := dl.ln6.Close()
	if err4 != nil {
		return err4
	}
	return err6
}

func (dl *dualStackListener) Addr() net.Addr {
	return dl.ln4.Addr()
}
