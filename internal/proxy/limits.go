package proxy

import (
	"log/slog"
	"net"
	"sync"
)

type concurrentLimiter struct {
	mu        sync.Mutex
	maxTotal  int
	maxPerKey int
	total     int
	perKey    map[string]int
}

func newConcurrentLimiter(maxTotal, maxPerKey int) *concurrentLimiter {
	return &concurrentLimiter{
		maxTotal:  maxTotal,
		maxPerKey: maxPerKey,
		perKey:    make(map[string]int),
	}
}

func (l *concurrentLimiter) tryAcquire(key string) (release func(), ok bool) {
	if key == "" {
		key = "unknown"
	}

	l.mu.Lock()
	if l.total >= l.maxTotal || l.perKey[key] >= l.maxPerKey {
		l.mu.Unlock()
		return nil, false
	}
	l.total++
	l.perKey[key]++
	l.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			l.total--
			l.perKey[key]--
			if l.perKey[key] == 0 {
				delete(l.perKey, key)
			}
			l.mu.Unlock()
		})
	}, true
}

type limitedListener struct {
	net.Listener
	limiter *concurrentLimiter
}

func newLimitedListener(listener net.Listener, maxConnections, maxConnectionsPerIP int) net.Listener {
	return &limitedListener{
		Listener: listener,
		limiter:  newConcurrentLimiter(maxConnections, maxConnectionsPerIP),
	}
}

func (l *limitedListener) Accept() (net.Conn, error) {
	for {
		connection, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		clientIP := addressHost(connection.RemoteAddr().String())
		release, ok := l.limiter.tryAcquire(clientIP)
		if !ok {
			slog.Warn("Connection limit exceeded", "remote", connection.RemoteAddr())
			if err := connection.Close(); err != nil {
				slog.Debug("Could not close rejected connection", "error", err)
			}
			continue
		}
		return &limitedConn{Conn: connection, release: release}, nil
	}
}

type limitedConn struct {
	net.Conn
	release func()
	once    sync.Once
}

func (c *limitedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

func (c *limitedConn) CloseWrite() error {
	if connection, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return connection.CloseWrite()
	}
	return nil
}

func addressHost(address string) string {
	host, _, err := net.SplitHostPort(address)
	if err == nil {
		return host
	}
	return address
}
