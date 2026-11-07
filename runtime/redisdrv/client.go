// Package redisdrv is a small Redis client written against the standard
// library: RESP2 over TCP or TLS, AUTH and SELECT, a bounded connection pool,
// and the handful of commands storm needs. Its Client is the Store that
// runtime/qcache caches query results in.
//
// Stdlib-only for the same reason runtime/mydrv speaks MySQL's wire itself:
// the packages a generated store links import nothing outside the standard
// library, so caching with Redis does not put a third-party client in an
// adopter's binary. An adopter who already runs go-redis or rueidis can
// satisfy qcache.Store with it instead and never import this package.
//
// It speaks to anything that speaks RESP2: Redis, Valkey, KeyDB, Dragonfly,
// and managed offerings of them. Cluster mode and Sentinel are not handled;
// point it at a primary or a proxy.
package redisdrv

import (
	"context"
	"strconv"
	"sync"
	"time"
	"unsafe"
)

// Client is a pool of connections to one server. It is safe for concurrent
// use.
type Client struct {
	opt  Options
	idle chan *conn
	sem  chan struct{}

	mu     sync.Mutex
	closed bool
}

// Open returns a client for o. It does not connect: the first command does,
// and Ping is how to find out at startup that the address is wrong.
func Open(o Options) *Client {
	o = o.withDefaults()
	return &Client{
		opt:  o,
		idle: make(chan *conn, o.MaxIdle),
		sem:  make(chan struct{}, o.MaxOpen),
	}
}

// OpenURL is Open(ParseURL(url)).
func OpenURL(url string) (*Client, error) {
	o, err := ParseURL(url)
	if err != nil {
		return nil, err
	}
	return Open(o), nil
}

// Ping checks that the server answers.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.Do(ctx, "PING")
	return err
}

// Get returns the value at key, or (nil, nil) when there is none.
func (c *Client) Get(ctx context.Context, key string) ([]byte, error) {
	r, err := c.Do(ctx, "GET", key)
	if err != nil {
		return nil, err
	}
	return bulk(r)
}

// MGet returns the values at keys in one round trip, a nil element for each
// missing key.
func (c *Client) MGet(ctx context.Context, keys []string) ([][]byte, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	r, err := c.Do(ctx, append([]string{"MGET"}, keys...)...)
	if err != nil {
		return nil, err
	}
	arr, ok := r.([]any)
	if !ok || len(arr) != len(keys) {
		return nil, errProtocol
	}
	out := make([][]byte, len(arr))
	for i, v := range arr {
		if out[i], err = bulk(v); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Set writes value at key, expiring after ttl (rounded up to a millisecond).
// A ttl of zero or less writes a key that does not expire.
func (c *Client) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	// Viewed, not copied: a cached result set can be large, and the command
	// is written out before Do returns, so nothing keeps the string past the
	// caller's slice.
	args := []string{"SET", key, unsafe.String(unsafe.SliceData(value), len(value))}
	if ttl > 0 {
		ms := (ttl + time.Millisecond - 1) / time.Millisecond
		args = append(args, "PX", strconv.FormatInt(int64(ms), 10))
	}
	_, err := c.Do(ctx, args...)
	return err
}

// Del removes keys and reports how many existed.
func (c *Client) Del(ctx context.Context, keys ...string) (int64, error) {
	if len(keys) == 0 {
		return 0, nil
	}
	r, err := c.Do(ctx, append([]string{"DEL"}, keys...)...)
	if err != nil {
		return 0, err
	}
	n, ok := r.(int64)
	if !ok {
		return 0, errProtocol
	}
	return n, nil
}

// Do sends one command and returns its reply: string, int64, []byte, []any,
// or nil. An error reply is returned as a *Error.
func (c *Client) Do(ctx context.Context, args ...string) (any, error) {
	cn, err := c.get(ctx)
	if err != nil {
		return nil, err
	}
	replies, err := cn.do(ctx, c.opt.IOTimeout, args)
	c.put(cn)
	if err != nil {
		return nil, err
	}
	if e, ok := replies[0].(*Error); ok {
		return nil, e
	}
	return replies[0], nil
}

// Close closes idle connections and refuses new commands. Commands in flight
// finish, and their connections are closed when they return.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	for {
		select {
		case cn := <-c.idle:
			cn.close()
		default:
			return nil
		}
	}
}

func (c *Client) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// get takes a slot and then a connection: an idle one if there is one, a new
// one if not.
func (c *Client) get(ctx context.Context) (*conn, error) {
	if c.isClosed() {
		return nil, ErrClosed
	}
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case cn := <-c.idle:
		return cn, nil
	default:
	}
	cn, err := dial(ctx, c.opt)
	if err != nil {
		<-c.sem
		return nil, err
	}
	return cn, nil
}

// put returns a connection and its slot. A bad one is closed, as is any the
// idle list has no room for.
func (c *Client) put(cn *conn) {
	defer func() { <-c.sem }()
	c.mu.Lock()
	defer c.mu.Unlock()
	if cn.bad || c.closed {
		cn.close()
		return
	}
	select {
	case c.idle <- cn:
	default:
		cn.close()
	}
}

func bulk(v any) ([]byte, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case []byte:
		return x, nil
	case string:
		return []byte(x), nil
	}
	return nil, errProtocol
}
