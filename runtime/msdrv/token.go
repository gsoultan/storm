package msdrv

import (
	"encoding/binary"
	"errors"
)

// The token stream: what every server reply is made of.
//
// A reply is a sequence of self-describing tokens, not a structure. A result
// set is COLMETADATA, then ROW or NBCROW repeated, then DONE — and an error, an
// informational message or an environment change may appear between any two of
// them. So this is a loop rather than a parser, and the caller drives it one
// token at a time.
//
// Two things about DONE are worth stating because getting either wrong
// desynchronises the connection. It carries the ROW COUNT, which is the only
// place an UPDATE's affected count appears. And it comes in three spellings —
// DONE, DONEPROC and DONEINPROC — where a statement sent through sp_executesql
// produces DONEINPROC for the statement and DONEPROC for the procedure, so a
// client that stops at the first one leaves a token on the wire.

const (
	tokReturnStatus  = 0x79
	tokColMetadata   = 0x81
	tokOrder         = 0xA9
	tokError         = 0xAA
	tokInfo          = 0xAB
	tokReturnValue   = 0xAC
	tokLoginAck      = 0xAD
	tokFeatureExtAck = 0xAE
	tokRow           = 0xD1
	tokNBCRow        = 0xD2
	tokEnvChange     = 0xE3
	tokDone          = 0xFD
	tokDoneProc      = 0xFE
	tokDoneInProc    = 0xFF
)

// DONE status bits.
const (
	doneFinal    = 0x00
	doneMore     = 0x01
	doneError    = 0x02
	doneInxact   = 0x04
	doneCount    = 0x10
	doneAttn     = 0x20
	doneSrvError = 0x100
)

// ENVCHANGE types. Only the ones that change how the client must behave.
const (
	envDatabase   = 1
	envLanguage   = 2
	envPacketSize = 4
	envBeginTxn   = 8
	envCommitTxn  = 9
	envRollback   = 10
	envTxnEnded   = 13
)

// readVarByte reads a one-byte-length binary value, which is what the
// transaction envelope changes carry.
func (x *conn) readVarByte() ([]byte, error) {
	n, err := x.readByte()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}
	b := make([]byte, n)
	if err := x.readFull(b); err != nil {
		return nil, err
	}
	return b, nil
}

// readLogin consumes the login reply.
//
// The interesting part is ENVCHANGE: the server confirms the packet size here,
// and a client that ignores it and keeps writing 4 KB packets to a server that
// agreed on 8 KB is not wrong, but one that ignores a SMALLER confirmed size
// writes packets the server will not read.
func (x *conn) readLogin() error {
	var first error
	for {
		t, err := x.readByte()
		if err != nil {
			if errors.Is(err, errEndOfMessage) {
				break
			}
			return err
		}
		switch t {
		case tokLoginAck:
			n, err := x.readU16()
			if err != nil {
				return err
			}
			if err := x.skip(int(n)); err != nil {
				return err
			}
		case tokEnvChange:
			if err := x.readEnvChange(); err != nil {
				return err
			}
		case tokInfo:
			if _, err := x.readMessage(); err != nil {
				return err
			}
		case tokError:
			e, err := x.readMessage()
			if err != nil {
				return err
			}
			if first == nil {
				first = e
			}
		case tokFeatureExtAck:
			if err := x.skipFeatureExtAck(); err != nil {
				return err
			}
		case tokDone, tokDoneProc, tokDoneInProc:
			if _, _, err := x.readDone(); err != nil {
				return err
			}
		default:
			return protoErr("unexpected token 0x%02X in the login reply", t)
		}
		if !x.inMsg {
			break
		}
	}
	if first != nil {
		return first
	}
	return nil
}

// readEnvChange applies an environment change.
func (x *conn) readEnvChange() error {
	n, err := x.readU16()
	if err != nil {
		return err
	}
	// The length covers the type byte and both values. Read the type, then the
	// two B_VARCHARs, then skip whatever is left — some change types carry
	// binary payloads this client does not act on, and skipping by the declared
	// length is what keeps the stream aligned when a server sends one.
	start := x.consumed()
	kind, err := x.readByte()
	if err != nil {
		return err
	}
	switch kind {
	case envPacketSize:
		newSize, err := x.readBVarchar()
		if err != nil {
			return err
		}
		if _, err := x.readBVarchar(); err != nil {
			return err
		}
		if v := atoiSafe(newSize); v > 0 {
			x.pending = v
		}
	case envDatabase, envLanguage:
		if _, err := x.readBVarchar(); err != nil {
			return err
		}
		if _, err := x.readBVarchar(); err != nil {
			return err
		}

	case envBeginTxn, envCommitTxn, envRollback, envTxnEnded:
		// These carry BYTES, with a one-byte length — not characters. Reading
		// them as a B_VARCHAR doubles the length and desynchronises the stream
		// at the first transaction.
		nv, err := x.readVarByte()
		if err != nil {
			return err
		}
		if _, err := x.readVarByte(); err != nil { // old value
			return err
		}
		if kind == envBeginTxn && len(nv) == 8 {
			x.txn = binary.LittleEndian.Uint64(nv)
		} else {
			x.txn = 0
		}

	default:
		// Unknown or binary: skip the declared remainder.
	}
	if rem := int(n) - (x.consumed() - start); rem > 0 {
		return x.skip(rem)
	}
	return nil
}

// skipFeatureExtAck walks the feature acknowledgements, which are a list
// terminated by 0xFF rather than a length-prefixed block.
func (x *conn) skipFeatureExtAck() error {
	for {
		id, err := x.readByte()
		if err != nil {
			return err
		}
		if id == 0xFF {
			return nil
		}
		n, err := x.readU32()
		if err != nil {
			return err
		}
		if err := x.skip(int(n)); err != nil {
			return err
		}
	}
}

// readDone reads a DONE token, returning its status and row count.
func (x *conn) readDone() (status uint16, rows int64, err error) {
	status, err = x.readU16()
	if err != nil {
		return 0, 0, err
	}
	if _, err = x.readU16(); err != nil { // current command
		return 0, 0, err
	}
	n, err := x.readU64()
	if err != nil {
		return 0, 0, err
	}
	if status&doneCount == 0 {
		// The count is only meaningful when the bit says so. Without this
		// check, a SELECT's DONE hands back whatever the field happened to hold
		// and an Exec reports rows it did not affect.
		return status, 0, nil
	}
	return status, int64(n), nil
}

// atoiSafe parses a small positive decimal, returning 0 for anything else.
func atoiSafe(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0
		}
		n = n*10 + int(s[i]-'0')
		if n > 1<<20 {
			return 0
		}
	}
	return n
}

// consumed is how many bytes of the current message have been read. Used to
// bound a token by its declared length rather than by trusting the parse.
func (x *conn) consumed() int { return x.nread + x.rpos }

// attention sends an Attention packet, which is TDS's cancellation.
//
// The server abandons the running statement and replies with a DONE carrying
// the attention bit, which the caller must still read — that is the whole
// protocol, and a client that sends an attention and then closes the socket
// leaves the server working on a statement nobody will read.
func (x *conn) attention() error {
	x.begin(pktAttention)
	return x.end()
}

// errIsEndOfMessage reports the sentinel the byte reader returns when a message
// is spent, without every caller importing errors for one comparison.
func errIsEndOfMessage(err error) bool { return errors.Is(err, errEndOfMessage) }
