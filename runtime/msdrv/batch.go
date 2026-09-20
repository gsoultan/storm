package msdrv

import (
	"context"
	"strconv"

	"github.com/gsoultan/storm/runtime"
)

// Batch: several statements, ONE round trip.
//
// This is where TDS is better than MySQL's protocol rather than worse.
// runtime/mydrv's Batch is N round trips and says so, because the MySQL wire
// has no pipeline. TDS does: an RPC request may carry several calls in one
// packet, separated by a batch flag, and the server answers with their token
// streams back to back. So a unit of work that writes twenty rows is one
// conversation.
//
// The ordering rule that comes with it: the replies arrive in the order the
// calls were sent, and each must be fully consumed before the next begins.
// A caller that abandons the i-th result has not skipped it — the bytes are
// still on the socket, and the (i+1)-th would be read as its continuation. So
// each op's rows are drained whether or not the callback reads them.

// batchFlag separates two RPC calls in one request.
const batchFlag = 0xFF

// Batch sends ops together and calls each with the i-th result.
func (c *Conn) Batch(ctx context.Context, ops []runtime.BatchOp,
	each func(i int, rows runtime.Rows, affected int64, err error) error) error {
	if len(ops) == 0 {
		return nil
	}
	if c.busy {
		return ErrRowsOpen
	}
	if c.bad {
		return protoErr("this connection's token stream is out of step and it cannot be reused")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	x := c.x
	x.begin(pktRPC)
	if err := x.writeHeaders(); err != nil {
		return err
	}
	for i := range ops {
		if err := c.writeCall(&ops[i]); err != nil {
			return err
		}
		// AFTER each call, the last one included. The grammar allows the
		// trailing flag to be omitted and the server does not agree: without it
		// the request is one call short of complete and the read blocks on a
		// reply that is never sent.
		if err := x.writeByte(batchFlag); err != nil {
			return err
		}
	}
	if err := x.end(); err != nil {
		return err
	}

	stop := c.watch(ctx)
	defer stop()

	// One result per op, in order. A BatchReader walks the shared stream and
	// hands out a view of each; nothing here materialises a result, because the
	// next one is behind it on the same socket.
	c.busy = true
	defer func() { c.busy = false }()

	started := false
	for i := range ops {
		if started && !x.inMsg {
			// The replies are spent before the ops are. A statement the server
			// refused to parse produces no reply of its own, so the counts can
			// differ — and asking for one more would block on a packet nobody
			// is going to send. The guard is on the SECOND op onwards, because
			// before the first read there is no message yet and "not in one" is
			// the normal state rather than the exhausted one.
			return protoErr("the server sent %d replies for %d statements", i, len(ops))
		}
		started = true
		r := &Rows{c: c, shared: true}
		err := r.header()
		var affected int64
		if ops[i].WantRows {
			if each != nil && err == nil {
				err = each(i, r, 0, nil)
			}
		} else {
			affected, err = r.drainCount(err)
			if each != nil {
				err = callEach(each, i, nil, affected, err)
			}
		}
		// Drained whether or not the callback looked: the (i+1)-th reply is
		// behind this one on the wire.
		r.finish()
		if err != nil {
			// The rest of the stream still has to go somewhere, or the
			// connection is unusable. Discarding it is cheaper than a reconnect
			// and is what makes an error in op 3 of 20 survivable.
			for j := i + 1; j < len(ops); j++ {
				rr := &Rows{c: c, shared: true}
				_ = rr.header()
				rr.finish()
			}
			if !c.bad {
				_ = x.drain()
			}
			return err
		}
	}
	if !c.bad {
		return x.drain()
	}
	return nil
}

func callEach(each func(int, runtime.Rows, int64, error) error,
	i int, rows runtime.Rows, affected int64, err error) error {
	if e := each(i, rows, affected, err); e != nil {
		return e
	}
	return err
}

// writeCall writes one RPC call: sp_executesql, or a plain statement when there
// is nothing to bind.
//
// A parameterless statement still goes through sp_executesql here, where a lone
// statement would go as a SQL batch. A batch packet and an RPC packet are
// different PACKET TYPES, so the two cannot be mixed in one request — and
// splitting the ops into two requests would be two round trips, which is the
// thing this method exists not to do.
func (c *Conn) writeCall(op *runtime.BatchOp) error {
	x := c.x
	if err := x.writeU16(0xFFFF); err != nil {
		return err
	}
	if err := x.writeU16(procExecuteSQL); err != nil {
		return err
	}
	if err := x.writeU16(0); err != nil {
		return err
	}
	if err := x.writeParamNVarCharMax("", op.SQL); err != nil {
		return err
	}
	decl, err := declare(op.Args)
	if err != nil {
		return err
	}
	if err := x.writeParamNVarCharMax("", decl); err != nil {
		return err
	}
	for i, a := range op.Args {
		if err := x.writeParam("@p"+strconv.Itoa(i+1), a); err != nil {
			return err
		}
	}
	return nil
}

// drainCount reads an op's reply for its affected count, discarding any rows.
func (r *Rows) drainCount(err error) (int64, error) {
	if err != nil {
		return 0, err
	}
	var affected int64
	for r.Next() {
	}
	affected = r.affected
	if e := r.Err(); e != nil && err == nil {
		err = e
	}
	return affected, err
}

// finish consumes the rest of THIS op's reply without ending the message, so
// the next op's tokens are where the next reader expects them.
func (r *Rows) finish() {
	if r.closed {
		return
	}
	r.closed = true
	if r.c.bad {
		return
	}
	for !r.done {
		if !r.Next() {
			break
		}
	}
	if !r.shared {
		r.c.busy = false
	}
}
