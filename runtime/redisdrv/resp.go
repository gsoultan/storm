package redisdrv

import (
	"bufio"
	"errors"
	"io"
	"strconv"
)

// RESP2, the protocol every Redis since 1.2 speaks and every compatible server
// implements. Commands go out as arrays of bulk strings; replies come back as
// one of five types.

func writeCommand(w *bufio.Writer, args []string) error {
	w.WriteByte('*')
	w.WriteString(strconv.Itoa(len(args)))
	w.WriteString("\r\n")
	for _, a := range args {
		w.WriteByte('$')
		w.WriteString(strconv.Itoa(len(a)))
		w.WriteString("\r\n")
		w.WriteString(a)
		if _, err := w.WriteString("\r\n"); err != nil {
			return err
		}
	}
	return nil
}

// readReply returns one reply as: string (simple string), *Error, int64,
// []byte or nil (bulk string), or []any or nil (array). A *Error is returned
// as the VALUE, not the error: err is reserved for the connection failing,
// which is what tells the pool to discard it.
//
// maxBytes is a budget for the WHOLE reply, not each part of it: every line
// and bulk string read is charged to it. Charging only each bulk string let an
// array of 64 Mi elements cost 1 GiB of []any from a 14-byte header, and an
// array of bulk strings cost maxBytes per element.
func readReply(r *bufio.Reader, maxBytes int) (any, error) {
	budget := maxBytes
	return readDepth(r, &budget, 0)
}

// maxDepth bounds nested arrays. No command this client sends gets back more
// than one level; the bound keeps a hostile server off the stack.
const maxDepth = 8

func readDepth(r *bufio.Reader, budget *int, depth int) (any, error) {
	line, err := readLine(r)
	if err != nil {
		return nil, err
	}
	if *budget -= len(line) + 2; *budget < 0 {
		return nil, errProtocol
	}
	if len(line) == 0 {
		return nil, errProtocol
	}
	body := line[1:]
	switch line[0] {
	case '+':
		return string(body), nil
	case '-':
		return &Error{Msg: string(body)}, nil
	case ':':
		n, err := strconv.ParseInt(string(body), 10, 64)
		if err != nil {
			return nil, errProtocol
		}
		return n, nil
	case '$':
		n, err := strconv.Atoi(string(body))
		if err != nil || n < -1 || n > *budget-2 {
			return nil, errProtocol
		}
		if n == -1 {
			return nil, nil
		}
		*budget -= n + 2
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		if buf[n] != '\r' || buf[n+1] != '\n' {
			return nil, errProtocol
		}
		return buf[:n:n], nil
	case '*':
		n, err := strconv.Atoi(string(body))
		// Every element is at least three bytes on the wire ("+\r\n"), so a
		// count the remaining budget cannot hold is refused before allocating.
		if err != nil || n < -1 || n > *budget/3 {
			return nil, errProtocol
		}
		if n == -1 {
			return nil, nil
		}
		if depth >= maxDepth {
			return nil, errProtocol
		}
		// Grown as elements arrive rather than sized from the header, so
		// memory follows bytes actually read and not a count the server claims.
		out := make([]any, 0, min(n, 1024))
		for range n {
			v, err := readDepth(r, budget, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	}
	return nil, errProtocol
}

func readLine(r *bufio.Reader) ([]byte, error) {
	line, err := r.ReadSlice('\n')
	if err != nil {
		if errors.Is(err, bufio.ErrBufferFull) {
			return nil, errProtocol
		}
		return nil, err
	}
	if len(line) < 2 || line[len(line)-2] != '\r' {
		return nil, errProtocol
	}
	return line[:len(line)-2], nil
}
