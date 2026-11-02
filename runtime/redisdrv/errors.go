package redisdrv

import "errors"

// Error is an error reply from the server: "-ERR …", "-WRONGTYPE …". The
// connection is still good after one.
type Error struct {
	Msg string
}

func (e *Error) Error() string { return "redis: " + e.Msg }

// ErrClosed is returned by a Client after Close.
var ErrClosed = errors.New("redisdrv: client is closed")

// errProtocol is a reply that does not parse. The connection it came from is
// discarded: nothing after it can be trusted to be aligned.
var errProtocol = errors.New("redisdrv: malformed reply")
