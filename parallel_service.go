package iproto

import (
	"sync"
)

type ParallelMiddleware struct {
	BasicResponder
	serv *ParallelService
	prev, next *ParallelMiddleware
	performed bool
}

func (p *ParallelMiddleware) Respond(res Response) Response {
	p.Cancel()
	return res
}

func (p *ParallelMiddleware) Cancel() {
	p.serv.Lock()
	p.prev.next = p.next
	p.next.prev = p.prev
	p.performed = true
	p.serv.Unlock()
}

type ParallelService struct {
	sync.Mutex
	work Service
	runned bool
	appended chan bool
	list ParallelMiddleware
	sema  chan bool
}

func NewParallelService(n int, work Service) (serv *ParallelService) {
	if n == 0 {
		n = 1
	}
	serv = &ParallelService {
		work: work,
		runned: true,
		appended: make(chan bool, 1),
		sema: make(chan bool, n),
	}
	serv.list.next = &serv.list
	serv.list.prev = &serv.list
	for i:=0; i<n; i++ {
		serv.sema <- true
	}
	go serv.loop()
	return
}

func (serv *ParallelService) Runned() bool {
	return serv.runned
}

func (serv *ParallelService) SendWrapped(r *Request) {
	serv.Lock()
	defer serv.Unlock()
	if serv.appended == nil {
		r.Respond(RcShutdown, nil)
		return
	}
	middle := &ParallelMiddleware {
		serv: serv,
		prev: serv.list.prev,
		next: &serv.list,
	}
	serv.list.prev.next = middle
	serv.list.prev = middle
	if !r.ChainMiddleware(middle) {
		return
	}
	select {
	case serv.appended <- true:
	}
}

func (serv *ParallelService) Send(r *Request) {
	serv.SendWrapped(r)
}

func (serv *ParallelService) loop() {
Loop:
	for {
		select {
		case app := <-serv.appended:
			if !app {
				serv.appended = nil
			}
			continue Loop
		case <-serv.sema:
		}

		if !serv.runOne() {
			serv.sema <- true
			if serv.appended == nil {
				break Loop
			}
			if app := <-serv.appended; !app {
				serv.appended = nil
			}
		}
	}
}

func (serv *ParallelService) runOne() bool {
	serv.Lock()
	defer serv.Unlock()

	next := serv.list.next
	if next == serv.list.next || next.performed {
		return false
	}

	next.prev.next = next.next
	next.next.prev = next.prev
	request := next.Request
	request.UnchainMiddleware(next)
	if !request.SetInFly(nil) {
		return false
	}
	go serv.runRequest(request)
	return true
}

func (serv *ParallelService) putSema() {
	serv.sema <- true
}

func (serv *ParallelService) runRequest(r *Request) {
	defer serv.putSema()
	serv.work.Send(r)
}
