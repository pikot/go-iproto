package server

import (
	"time"

	"github.com/funny-falcon/go-iproto"
	"github.com/funny-falcon/go-iproto/net"
)

type Config struct {
	Network string
	Address string

	EndPoint iproto.Service

	ReadTimeout  time.Duration
	WriteTimeout time.Duration

	RCType net.RCType
	RCMap  map[iproto.RetCode]iproto.RetCode
}
