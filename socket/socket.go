// Socket package provides a concise, powerful and high-performance TCP socket.
//
// Copyright 2017 HenryLee. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
package socket

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/henrylee2cn/goutil"
	"github.com/henrylee2cn/goutil/errors"
	"github.com/henrylee2cn/teleport/codec"
)

type (
	// Socket is a generic stream-oriented network connection.
	//
	// Multiple goroutines may invoke methods on a Socket simultaneously.
	Socket interface {
		// LocalAddr returns the local network address.
		LocalAddr() net.Addr

		// RemoteAddr returns the remote network address.
		RemoteAddr() net.Addr

		// SetDeadline sets the read and write deadlines associated
		// with the connection. It is equivalent to calling both
		// SetReadDeadline and SetWriteDeadline.
		//
		// A deadline is an absolute time after which I/O operations
		// fail with a timeout (see type Error) instead of
		// blocking. The deadline applies to all future and pending
		// I/O, not just the immediately following call to Read or
		// Write. After a deadline has been exceeded, the connection
		// can be refreshed by setting a deadline in the future.
		//
		// An idle timeout can be implemented by repeatedly extending
		// the deadline after successful Read or Write calls.
		//
		// A zero value for t means I/O operations will not time out.
		SetDeadline(t time.Time) error

		// SetReadDeadline sets the deadline for future Read calls
		// and any currently-blocked Read call.
		// A zero value for t means Read will not time out.
		SetReadDeadline(t time.Time) error

		// SetWriteDeadline sets the deadline for future Write calls
		// and any currently-blocked Write call.
		// Even if write times out, it may return n > 0, indicating that
		// some of the data was successfully written.
		// A zero value for t means Write will not time out.
		SetWriteDeadline(t time.Time) error

		// WritePacket writes header and body to the connection.
		// Note: must be safe for concurrent use by multiple goroutines.
		WritePacket(packet *Packet) error

		// ReadPacket reads header and body from the connection.
		// Note: must be safe for concurrent use by multiple goroutines.
		ReadPacket(packet *Packet) error

		// Read reads data from the connection.
		// Read can be made to time out and return an Error with Timeout() == true
		// after a fixed time limit; see SetDeadline and SetReadDeadline.
		Read(b []byte) (n int, err error)

		// Write writes data to the connection.
		// Write can be made to time out and return an Error with Timeout() == true
		// after a fixed time limit; see SetDeadline and SetWriteDeadline.
		Write(b []byte) (n int, err error)

		// Reset reset net.Conn
		// Reset(net.Conn)

		// Close closes the connection socket.
		// Any blocked Read or Write operations will be unblocked and return errors.
		Close() error

		// Public returns temporary public data of Socket.
		Public() goutil.Map
		// PublicLen returns the length of public data of Socket.
		PublicLen() int
		// Id returns the socket id.
		Id() string
		// SetId sets the socket id.
		SetId(string)
		// Reset reset net.Conn and ProtoFunc.
		Reset(netConn net.Conn, protoFunc ...ProtoFunc)
	}
	socket struct {
		net.Conn
		protocol  Proto
		id        string
		idMutex   sync.RWMutex
		ctxPublic goutil.Map
		mu        sync.RWMutex
		curState  int32
		fromPool  bool
	}
)

const (
	normal      int32 = 0
	activeClose int32 = 1
)

var _ net.Conn = Socket(nil)

// ErrProactivelyCloseSocket proactively close the socket error.
var ErrProactivelyCloseSocket = errors.New("socket is closed proactively")

// GetSocket gets a Socket from pool, and reset it.
func GetSocket(c net.Conn, protoFunc ...ProtoFunc) Socket {
	s := socketPool.Get().(*socket)
	s.Reset(c, protoFunc...)
	return s
}

var socketPool = sync.Pool{
	New: func() interface{} {
		s := newSocket(nil, nil)
		s.fromPool = true
		return s
	},
}

// NewSocket wraps a net.Conn as a Socket.
func NewSocket(c net.Conn, protoFunc ...ProtoFunc) Socket {
	return newSocket(c, protoFunc)
}

func newSocket(c net.Conn, protoFuncs []ProtoFunc) *socket {
	var s = &socket{
		protocol: getProto(protoFuncs, c),
		Conn:     c,
	}
	s.optimize()
	return s
}

// WritePacket writes header and body to the connection.
// WritePacket can be made to time out and return an Error with Timeout() == true
// after a fixed time limit; see SetDeadline and SetWriteDeadline.
// Note:
//  For the byte stream type of body, write directly, do not do any processing;
//  Must be safe for concurrent use by multiple goroutines.
func (s *socket) WritePacket(packet *Packet) error {
	s.mu.RLock()
	protocol := s.protocol
	s.mu.RUnlock()
	if packet.BodyCodec() == codec.NilCodecId {
		packet.SetBodyCodec(defaultBodyCodec.Id())
	}
	err := protocol.Pack(packet)
	if err != nil && s.isActiveClosed() {
		err = ErrProactivelyCloseSocket
	}
	return err
}

// ReadPacket reads header and body from the connection.
// Note:
//  For the byte stream type of body, read directly, do not do any processing;
//  Must be safe for concurrent use by multiple goroutines.
func (s *socket) ReadPacket(packet *Packet) error {
	s.mu.RLock()
	protocol := s.protocol
	s.mu.RUnlock()
	return protocol.Unpack(packet)
}

// Public returns temporary public data of Socket.
func (s *socket) Public() goutil.Map {
	if s.ctxPublic == nil {
		s.ctxPublic = goutil.RwMap()
	}
	return s.ctxPublic
}

// PublicLen returns the length of public data of Socket.
func (s *socket) PublicLen() int {
	if s.ctxPublic == nil {
		return 0
	}
	return s.ctxPublic.Len()
}

// Id returns the socket id.
func (s *socket) Id() string {
	s.idMutex.RLock()
	id := s.id
	if len(id) == 0 {
		id = s.RemoteAddr().String()
	}
	s.idMutex.RUnlock()
	return id
}

// SetId sets the socket id.
func (s *socket) SetId(id string) {
	s.idMutex.Lock()
	s.id = id
	s.idMutex.Unlock()
}

// Reset reset net.Conn and ProtoFunc.
func (s *socket) Reset(netConn net.Conn, protoFunc ...ProtoFunc) {
	atomic.StoreInt32(&s.curState, activeClose)
	if s.Conn != nil {
		s.Conn.Close()
	}
	s.mu.Lock()
	s.Conn = netConn
	s.SetId("")
	s.protocol = getProto(protoFunc, netConn)
	atomic.StoreInt32(&s.curState, normal)
	s.optimize()
	s.mu.Unlock()
}

// Close closes the connection socket.
// Any blocked Read or Write operations will be unblocked and return errors.
// If it is from 'GetSocket()' function(a pool), return itself to pool.
func (s *socket) Close() error {
	if s.isActiveClosed() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.isActiveClosed() {
		return nil
	}
	atomic.StoreInt32(&s.curState, activeClose)

	var err error
	if s.Conn != nil {
		err = s.Conn.Close()
	}
	if s.fromPool {
		s.Conn = nil
		s.ctxPublic = nil
		s.protocol = nil
		socketPool.Put(s)
	}
	return err
}

func (s *socket) isActiveClosed() bool {
	return atomic.LoadInt32(&s.curState) == activeClose
}

func (s *socket) optimize() {
	if c, ok := s.Conn.(ifaceSetKeepAlive); ok {
		if changeKeepAlive {
			c.SetKeepAlive(keepAlive)
		}
		if keepAlive && keepAlivePeriod >= 0 {
			c.SetKeepAlivePeriod(keepAlivePeriod)
		}
	}
	if c, ok := s.Conn.(ifaceSetBuffer); ok {
		if readBuffer >= 0 {
			c.SetReadBuffer(readBuffer)
		}
		if writeBuffer >= 0 {
			c.SetWriteBuffer(writeBuffer)
		}
	}
}

type (
	ifaceSetKeepAlive interface {
		// SetKeepAlive sets whether the operating system should send
		// keepalive messages on the connection.
		SetKeepAlive(keepalive bool) error
		// SetKeepAlivePeriod sets period between keep alives.
		SetKeepAlivePeriod(d time.Duration) error
	}
	ifaceSetBuffer interface {
		// SetReadBuffer sets the size of the operating system's
		// receive buffer associated with the connection.
		SetReadBuffer(bytes int) error
		// SetWriteBuffer sets the size of the operating system's
		// transmit buffer associated with the connection.
		SetWriteBuffer(bytes int) error
	}
)

// Connection related system configuration
var (
	writeBuffer     int           = -1
	readBuffer      int           = -1
	changeKeepAlive bool          = false
	keepAlive       bool          = true
	keepAlivePeriod time.Duration = -1
)

// SetKeepAlive sets whether the operating system should send
// keepalive messages on the connection.
// Note: If have not called the function, the system defaults are used.
func SetKeepAlive(keepalive bool) {
	changeKeepAlive = true
	keepAlive = keepalive
}

// SetKeepAlivePeriod sets period between keep alives.
// Note: if d=-1, don't change the system default value.
func SetKeepAlivePeriod(d time.Duration) {
	keepAlivePeriod = d
}

// SetReadBuffer sets the size of the operating system's
// receive buffer associated with the connection.
// Note: if bytes=-1, don't change the system default value.
func SetReadBuffer(bytes int) {
	readBuffer = bytes
	resetFastProtoReadBufioSize()
}

// SetWriteBuffer sets the size of the operating system's
// transmit buffer associated with the connection.
// Note: if bytes=-1, don't change the system default value.
func SetWriteBuffer(bytes int) {
	writeBuffer = bytes
}

var fastProtoReadBufioSize int

func init() {
	resetFastProtoReadBufioSize()
}

func resetFastProtoReadBufioSize() {
	if readBuffer < 0 {
		fastProtoReadBufioSize = 1024 * 4
	} else if readBuffer == 0 {
		fastProtoReadBufioSize = 1024 * 35
	} else {
		fastProtoReadBufioSize = readBuffer / 2
	}
}

func getProto(protoFuncs []ProtoFunc, rw io.ReadWriter) Proto {
	if len(protoFuncs) > 0 {
		return protoFuncs[0](rw)
	} else {
		return defaultProtoFunc(rw)
	}
}
