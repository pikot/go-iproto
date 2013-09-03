package server

import (
	"github.com/funny-falcon/go-iproto"
	nt "github.com/funny-falcon/go-iproto/net"
	"log"
	"sync"
)

var _ = log.Print

type notifyAction uint32

const (
	writeClosed = notifyAction(iota + 1)
	readClosed
	inFlyEmpty
)

type ConnState uint32

const (
	CsConnected = ConnState(1 << iota)
	CsReadClosed
	CsWriteClosed
	CsClosed = CsReadClosed | CsWriteClosed
)

type Connection struct {
	*Server
	Id   uint64
	conn nt.NetConn

	buf        []nt.Response
	out        chan nt.Response
	bufRealCap int

	state ConnState

	inFly map[uint32]*iproto.Request
	sync.Mutex

	loopNotify chan notifyAction
}

func NewConnection(serv *Server, connection nt.NetConn, id uint64) (conn *Connection) {
	log.Print("New connection ", id, connection.RemoteAddr())
	conn = &Connection{
		Server: serv,
		Id:     id,
		conn:   connection,

		out: make(chan nt.Response, 128),

		inFly: make(map[uint32]*iproto.Request),

		state: CsConnected,

		loopNotify: make(chan notifyAction, 2),
	}
	return
}

func (conn *Connection) Run() {
	go conn.controlLoop()
	go conn.readLoop()
	go conn.writeLoop()
}

func (conn *Connection) Stop() {
	conn.conn.CloseRead()
}

func (conn *Connection) Respond(r *iproto.Response) {
	var ok bool
	conn.Lock()
	if _, ok = conn.inFly[r.Id]; ok {
		delete(conn.inFly, r.Id)
		select {
		case conn.out <- nt.Response(*r):
		default:
			conn.buf = append(conn.buf, nt.Response(*r))
			conn.bufRealCap = cap(conn.buf)
		}
	}
	conn.Unlock()
}

func (conn *Connection) cancelInFly() {
	log.Print("Canceling ", len(conn.inFly), " requests ", conn.Id, conn.conn.RemoteAddr())
	conn.Lock()
	reqs := make([]*iproto.Request, 0, len(conn.inFly))
	for _, req := range conn.inFly {
		reqs = append(reqs, req)
	}
	conn.Unlock()
	for _, req := range reqs {
		req.Cancel()
	}
	conn.Server.connClosed <- conn.Id
}

func (conn *Connection) closed() {
	log.Print("Closed ", conn.Id, conn.conn.RemoteAddr())
	conn.buf = nil
	conn.inFly = nil
	conn.Server.connClosed <- conn.Id
}

func (conn *Connection) controlLoop() {
	defer conn.closed()
	for {
		action := <-conn.loopNotify
		switch action {
		case readClosed:
			conn.Lock()
			conn.state &= CsClosed
			conn.state |= CsReadClosed
			log.Print("Read Closed ", conn.Id, conn.state, conn.conn.RemoteAddr())
			if len(conn.inFly)+len(conn.buf)+len(conn.out) == 0 {
				close(conn.out)
			}
			conn.Unlock()
		case writeClosed:
			conn.state &= CsClosed
			conn.state |= CsWriteClosed
			log.Print("Write Closed ", conn.Id, conn.state, conn.conn.RemoteAddr(), CsClosed)
			conn.cancelInFly()
			if conn.state&CsReadClosed == 0 {
				conn.conn.CloseRead()
			}
		case inFlyEmpty:
			close(conn.out)
		}

		if conn.state&CsClosed == CsClosed {
			log.Print("Breaking control loop")
			break
		}
	}
}

func (conn *Connection) notifyLoop(action notifyAction) {
	conn.loopNotify <- action
}

func (conn *Connection) readLoop() {
	var req nt.Request
	var err error
	var r nt.HeaderReader
	r.Init(conn.conn, conn.ReadTimeout)

	defer conn.notifyLoop(readClosed)

	var buf *[16]iproto.Request
	var bufn int

	for {
		if req, err = r.ReadRequest(); err != nil {
			break
		}

		if req.Msg == iproto.Ping {
			res := nt.Response{
				Id:  req.Id,
				Msg: iproto.Ping,
			}
			conn.out <- res
			continue
		}

		if buf == nil {
			buf = &[16]iproto.Request{}
		}
		request := &buf[bufn]
		if bufn++; bufn == len(buf) {
			buf = nil
			bufn = 0
		}

		*request = iproto.Request{
			Id:        req.Id,
			Msg:       req.Msg,
			Body:      req.Body,
			Responder: conn,
		}
		//request.SetPending()
		conn.Lock()
		conn.inFly[request.Id] = request
		conn.Unlock()

		conn.EndPoint.Send(request)
	}
}

func (conn *Connection) cleanBuffer() (hasOne bool) {
	n := 0
Loop:
	for n < len(conn.buf) {
		select {
		case conn.out <- conn.buf[n]:
			conn.buf[n] = nt.Response{}
			n++
		default:
			hasOne = true
			break Loop
		}
	}
	if n > 0 {
		hasOne = true
		conn.buf = conn.buf[n:]
		if len(conn.buf) == 0 {
			conn.bufRealCap = 16
			conn.buf = make([]nt.Response, 0, 16)
		} else if len(conn.buf) < conn.bufRealCap/16 {
			conn.bufRealCap = len(conn.buf) * 2
			tmp := make([]nt.Response, len(conn.buf), conn.bufRealCap)
			copy(tmp, conn.buf)
			conn.buf = tmp
		}
	}
	return
}

func (conn *Connection) writeLoop() {
	var err error
	var w nt.HeaderWriter

	w.Init(conn.conn, conn.WriteTimeout)

	defer func() {
		if err == nil {
			if err = w.Flush(); err == nil {
				conn.conn.CloseWrite()
			}
		}
		conn.notifyLoop(writeClosed)
	}()

Loop:
	for {
		var res nt.Response
		var ok bool

	Select:
		select {
		case res, ok = <-conn.out:
			if !ok {
				break Loop
			}
		default:
			conn.Lock()
			if len(conn.buf) > 0 {
				if conn.cleanBuffer() {
					conn.Unlock()
					goto Select
				}
			} else if conn.state&CsReadClosed != 0 && len(conn.inFly) == 0 && len(conn.out) == 0 {
				conn.Unlock()
				conn.notifyLoop(inFlyEmpty)
				break Loop
			}
			conn.Unlock()
			if err = w.Flush(); err != nil {
				break Loop
			}
			if res, ok = <-conn.out; !ok {
				break Loop
			}
		}

		if err = w.WriteResponse(res, conn.RetCodeLen); err != nil {
			break Loop
		}
	}
}
