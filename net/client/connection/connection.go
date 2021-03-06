package connection

import (
	"io"
	"log"
	"net"
	"sync"
	"time"

	"github.com/funny-falcon/go-iproto"
	nt "github.com/funny-falcon/go-iproto/net"
)

var _ = log.Print

type notifyAction uint32

const (
	writeClosed = notifyAction(iota + 1)
	readClosed
	readEmpty
)

type ErrorWhen uint8

const (
	Dial = ErrorWhen(iota + 1)
	Read
	Write
)

type Error struct {
	Conn  *Connection
	When  ErrorWhen
	Error error
}

type ConnState uint32

const (
	CsNew = 1 << iota
	CsDialing
	CsConnected
	CsReadClosed
	CsWriteClosed
	CsClosed = CsReadClosed | CsWriteClosed
)

type CConf struct {
	Network string
	Address string

	PingInterval time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	DialTimeout  time.Duration

	RetCodeType nt.RCType

	ConnErr chan<- Error
}

type Connection struct {
	iproto.SimplePoint
	Id uint64
	sync.Mutex
	*CConf

	conn nt.NetConn

	closeWrite chan bool
	readErr    error

	inFly RequestHolder

	State    ConnState
	shutdown bool

	loopNotify chan notifyAction

	reader nt.HeaderReader
	writer nt.HeaderWriter
}

var _ iproto.EndPoint = (*Connection)(nil)

func NewConnection(conf *CConf, id uint64) (conn *Connection) {
	conn = &Connection{
		CConf: conf,
		Id:    id,

		loopNotify: make(chan notifyAction, 2),
		State:      CsNew,
	}
	conn.inFly.init()
	conn.SimplePoint.Init(conn)
	return
}

/* default 5 seconds interval for Connection */
const DialTimeout = 5 * time.Second

func (conn *Connection) Loop() {
	dialer := net.Dialer{Timeout: DialTimeout}
	conn.State = CsDialing
	if netconn, err := dialer.Dial(conn.Network, conn.Address); err != nil {
		conn.ConnErr <- Error{conn, Dial, err}
		conn.State = CsClosed
	} else {
		conn.conn = netconn.(nt.NetConn)
		conn.reader.Init(conn.conn, conn.ReadTimeout, conn.RetCodeType)
		conn.writer.Init(conn.conn, conn.WriteTimeout, conn.RetCodeType)
		if err = conn.writer.Ping(); err == nil {
			if err = conn.writer.Flush(); err == nil {
				err = conn.reader.ReadPing()
			}
		}
		if err != nil {
			conn.conn.Close()
			conn.ConnErr <- Error{conn, Dial, err}
			conn.State = CsClosed
			return
		}
		conn.ConnErr <- Error{conn, Dial, nil}
		conn.State = CsConnected
		go conn.readLoop()
		go conn.writeLoop()
		go conn.controlLoop()
	}
}

/* RunWithConn is for testing purposes */
func (conn *Connection) RunWithConn(netconn io.ReadWriteCloser) {
	switch nc := netconn.(type) {
	case nt.NetConn:
		conn.conn = nc
	default:
		conn.conn = nt.RwcWrapper{ReadWriteCloser: netconn}
	}
	conn.ConnErr <- Error{conn, Dial, nil}
	go conn.readLoop()
	go conn.writeLoop()
	conn.controlLoop()
}

func (conn *Connection) controlLoopExit() {
	if conn.State&CsWriteClosed == 0 {
		conn.conn.CloseWrite()
	}
	conn.ConnErr <- Error{conn, Read, conn.readErr}
	conn.flushInFly()
}

func (conn *Connection) controlLoop() {
	var closeReadCalled bool
	defer conn.controlLoopExit()
	for {
		action := <-conn.loopNotify

		switch action {
		case writeClosed:
			conn.State &= CsClosed
			conn.State |= CsWriteClosed
		case readClosed:
			conn.State &= CsClosed
			conn.State |= CsReadClosed
			if conn.State&CsWriteClosed == 0 {
				conn.conn.CloseWrite()
			}
		case readEmpty:
		}

		if conn.State&CsWriteClosed != 0 {
			if !closeReadCalled && conn.inFly.count() == 0 {
				conn.conn.CloseRead()
				closeReadCalled = true
			}
			if conn.State&CsReadClosed != 0 {
				break
			}
		}
	}
}

func (conn *Connection) flushInFly() {
	reqs := conn.inFly.getAll()
	conn.inFly = RequestHolder{}

	code := iproto.RcIOError
	if conn.shutdown {
		code = iproto.RcShutdown
	}
	for _, req := range reqs {
		if request := req.Request; request != nil {
			request.RespondFail(code)
		}
	}
}

func (conn *Connection) notifyLoop(action notifyAction) {
	conn.loopNotify <- action
}

func (conn *Connection) readLoop() {
	var res nt.Response
	r := conn.reader

	defer conn.notifyLoop(readClosed)

	for {
		if res, conn.readErr = r.ReadResponse(); conn.readErr != nil {
			break
		}

		if res.Id == iproto.PingRequestId && res.Msg == iproto.Ping {
			continue
		}

		if ireq := conn.inFly.remove(res.Id); ireq != nil {
			ireq.RespondBytes(res.Code, res.Body)
		}
		if conn.State&CsWriteClosed != 0 && conn.inFly.put >= conn.inFly.got {
			conn.notifyLoop(readEmpty)
		}
	}
}

const fakePingInterval = 1 * time.Hour

func (conn *Connection) writeLoop() {
	var err error
	var pingTicker *time.Ticker

	w := conn.writer

	if conn.PingInterval > 0 {
		pingTicker = time.NewTicker(conn.PingInterval)
	} else {
		pingTicker = time.NewTicker(fakePingInterval)
		pingTicker.Stop()
	}

	defer func() {
		pingTicker.Stop()
		if err == nil {
			if err = w.Flush(); err == nil {
				conn.conn.CloseWrite()
			}
		}
		conn.notifyLoop(writeClosed)
		conn.ConnErr <- Error{conn, Write, err}
	}()

	var req *Request

Loop:
	for {
		var request *iproto.Request
		var ping bool
		var requestHeader nt.Request

		if conn.Stopped() {
			conn.shutdown = true
			break Loop
		}

		select {
		case request = <-conn.ReceiveChan():
		default:
			if err = w.Flush(); err != nil {
				break Loop
			}
			select {
			case <-pingTicker.C:
				ping = true
			case request = <-conn.ReceiveChan():
			case <-conn.ExitChan():
				conn.shutdown = true
				break Loop
			}
		}

		if ping {
			err = w.Ping()
		} else {
			if req == nil {
				req = conn.inFly.getNext(conn)
			}
			if !request.SetInFly(req) {
				continue
			}
			request.Lock()
			if !request.IsInFly() {
				request.Unlock()
				continue
			}
			requestHeader = nt.Request{
				Msg:  request.Msg,
				Id:   req.fakeId,
				Body: request.Body,
			}
			request.Unlock()

			req = nil

			err = w.WriteRequest(requestHeader)
		}
		if err != nil {
			break
		}
	}
}

func (conn *Connection) Closed() bool {
	return conn.State&CsClosed != 0
}

func (conn *Connection) LocalAddr() net.Addr {
	return conn.conn.LocalAddr()
}

func (conn *Connection) RemoteAddr() net.Addr {
	return conn.conn.RemoteAddr()
}
