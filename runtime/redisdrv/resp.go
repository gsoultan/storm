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
func readReply(r *bufio.Reader, maxBytes int) (any, error) { return readDepth(r, maxBytes, 0) }

// maxDepth bounds nested arrays. No command this client sends gets back more
// than one level; the bound keeps a hostile server off the stack.
const maxDepth = 8

func readDepth(r *bufio.Reader, maxBytes, depth int) (any, error) {
	line, err := readLine(r)
	if err != nil {
		return nil, err
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
		if err != nil || n < -1 || n > maxBytes {
			return nil, errProtocol
		}
		if n == -1 {
			return nil, nil
		}
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
		if err != nil || n < -1 || n > maxBytes {
			return nil, errProtocol
		}
		if n == -1 {
			return nil, nil
		}
		if depth >= maxDepth {
			return nil, errProtocol
		}
		out := make([]any, n)
		for i := range out {
			if out[i], err = readDepth(r, maxBytes, depth+1); err != nil {
				return nil, err
			}
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
