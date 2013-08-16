package connection

import (
	"github.com/funny-falcon/go-iproto"
	"sync"
	"github.com/funny-falcon/go-iproto/util"
	"log"
)

type Request struct {
	iproto.BasicResponder
	conn *Connection
	fakeId uint32
}

func wrapRequest(conn *Connection, ireq *iproto.Request, id uint32) *Request {
	req := &Request {
		conn: conn,
		fakeId: id,
	}
	req.Chain(ireq)
	return req
}

func (r *Request) Respond(res iproto.Response) {
	prev := r.Unchain()
	if prev != nil {
		res.Id = r.Request.Id
		prev.Respond(res)
	}
}

func (r *Request) Cancel() {
	prev := r.Unchain()
	if prev != nil {
		prev.Cancel()
	}
}

const (
	rowLogN = 8
	rowN = 1 << rowLogN
	rowMask, rowN1 = rowN-1, rowN-1
)

type RequestRow struct {
	used util.Atomic
	freed util.Atomic
	reqs  [rowN]Request
}

type reqMap map[util.Atomic]*RequestRow
type RequestHolder struct {
	sync.RWMutex
	count util.Atomic
	curId util.Atomic
	reqs reqMap
}

func (h *RequestHolder) getNext(conn *Connection) *Request {
	h.count.Incr()
	for {
		var reqs *RequestRow
		var ok bool
		id := h.curId.Incr()
		big := id>>rowLogN
		h.RLock()
		reqs, ok = h.reqs[big]
		h.RUnlock()
		if !ok {
			h.Lock()
			if reqs, ok = h.reqs[big]; !ok {
				reqs = &RequestRow{}
				h.reqs[big] = reqs
			}
			h.Unlock()
		}
		if id != 0 && id != util.Atomic(iproto.PingRequestId) {
			req := &reqs.reqs[id&rowMask]
			if req.conn != nil {
				continue
			}
			req.conn = conn
			req.fakeId = uint32(id)
			reqs.used.Incr()
			return req
		}
	}
}

func (h *RequestHolder) get(id uint32) *Request {
	h.RLock()
	defer h.RUnlock()
	var reqs *RequestRow
	var ok bool
	big := util.Atomic(id>>rowLogN)
	if reqs, ok = h.reqs[big]; !ok {
		log.Panicf("Map has no RequestRow for %d", id)
	}
	return &reqs.reqs[id&rowMask]
}

func (h *RequestHolder) putBack(r *Request) {
	if r.fakeId != 0 {
		var reqs *RequestRow
		var ok bool
		big := util.Atomic(r.fakeId>>rowLogN)
		h.RLock()
		if reqs, ok = h.reqs[big]; !ok {
			log.Panicf("Map has no RequestRow for %d", r.fakeId)
		}
		h.RUnlock()
		reqs.reqs[r.fakeId&rowMask] = Request{}
		border := big == 0 || big == util.Atomic(iproto.PingRequestId>>8)
		freed := reqs.freed.Incr()
		if freed == rowN || (freed == rowN1 && border) {
			h.Lock()
			delete(h.reqs, big)
			h.Unlock()
		}
		h.count.Decr()
	}
}

func (h *RequestHolder) getAll() (reqs []*iproto.Request) {
	h.Lock()
	defer h.Unlock()
	reqs = make([]*iproto.Request, h.count)
	i := 0
	for _, row := range h.reqs {
		for _, req := range row.reqs {
			if req.conn != nil {
				reqs[i] = req.Request
				i++
			}
		}
	}
	return
}
