// Package radix is a simple redis driver. It needs better docs
package radix

import (
	"bufio"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/mediocregopher/radix.v2/resp"
)

// Client describes an entity which can carry out Actions, e.g. a connection
// pool for a single redis instance or the cluster client.
type Client interface {
	// Do performs an Action, returning any error. A Client's Do method will
	// always be thread-safe.
	Do(Action) error

	// Once Close() is called all future method calls on the Client will return
	// an error
	Close() error
}

// ClientFunc is a function which can be used to create a Client for a single
// redis instance on the given network/address.
type ClientFunc func(network, addr string) (Client, error)

// DefaultClientFunc is a ClientFunc which will return a Client for a redis
// instance using sane defaults. This is used in this package when
var DefaultClientFunc = func(network, addr string) (Client, error) {
	return NewPool(network, addr, 20, nil)
}

// Conn is a Client wrapping a single network connection which synchronously
// reads/writes data using the redis resp protocol.
type Conn interface {
	Client

	// Encode and Decode may be called at the same time by two different
	// go-routines, but each should only be called once at a time (i.e. two
	// routines shouldn't call Encode at the same time, same with Decode).
	//
	// Encode and Decode should _not_ be called at the same time as Do.
	//
	// If either Encode or Decode encounter a net.Error the Conn will be
	// automatically closed.
	Encode(resp.Marshaler) error
	Decode(resp.Unmarshaler) error

	// Returns the underlying network connection, as-is. Read, Write, and Close
	// should not be called on the returned Conn.
	NetConn() net.Conn
}

// a wrapper around net.Conn which prevents Read, Write, and Close from being
// called
type connLimited struct {
	net.Conn
}

func (cl connLimited) Read(b []byte) (int, error) {
	return 0, errors.New("Read not allowed to be called on net.Conn returned from radix")
}

func (cl connLimited) Write(b []byte) (int, error) {
	return 0, errors.New("Write not allowed to be called on net.Conn returned from radix")
}

func (cl connLimited) Close() error {
	return errors.New("Close not allowed to be called on net.Conn returned from radix")
}

type connWrap struct {
	net.Conn
	brw *bufio.ReadWriter
	doL sync.Mutex
}

// NewConn takes an existing net.Conn and wraps it to support the Conn interface
// of this package. The Read and Write methods on the original net.Conn should
// not be used after calling this method.
//
// In both the Encode and Decode methods of the returned Conn, if a net.Error is
// encountered the Conn will have Close called on it automatically.
func NewConn(conn net.Conn) Conn {
	return &connWrap{
		Conn: conn,
		brw:  bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn)),
	}
}

func (cw *connWrap) Do(a Action) error {
	cw.doL.Lock()
	defer cw.doL.Unlock()
	// the action may want to call Do on the Conn (possibly more than once), but
	// if we passed in this connWrap as-is it would be locked and that wouldn't
	// be possible. By making an inner one we can let the outer one stay locked,
	// and the inner one's Do calls will lock themselves correctly as well.
	inner := &connWrap{
		Conn: cw.Conn,
		brw:  cw.brw,
	}
	return a.Run(inner)
}

func (cw *connWrap) Encode(m resp.Marshaler) error {
	err := m.MarshalRESP(cw.brw)
	defer func() {
		if _, ok := err.(net.Error); ok {
			cw.Close()
		}
	}()

	if err != nil {
		return err
	}
	err = cw.brw.Flush()
	return err
}

func (cw *connWrap) Decode(u resp.Unmarshaler) error {
	err := u.UnmarshalRESP(cw.brw.Reader)
	if _, ok := err.(net.Error); ok {
		cw.Close()
	}
	return err
}

func (cw *connWrap) NetConn() net.Conn {
	return connLimited{cw.Conn}
}

// ConnFunc is a function which returns an initialized, ready-to-be-used Conn.
// Functions like NewPool or NewCluster take in a ConnFunc in order to allow for
// things like calls to AUTH on each new connection, setting timeouts, custom
// Conn implementations, etc...
type ConnFunc func(network, addr string) (Conn, error)

// Dial is a ConnFunc creates a network connection using net.Dial and passes it
// into NewConn.
func Dial(network, addr string) (Conn, error) {
	c, err := net.Dial(network, addr)
	if err != nil {
		return nil, err
	}
	return NewConn(c), nil
}

type timeoutConn struct {
	net.Conn
	timeout time.Duration
}

func (tc *timeoutConn) setDeadline() {
	if tc.timeout > 0 {
		tc.Conn.SetDeadline(time.Now().Add(tc.timeout))
	}
}

func (tc *timeoutConn) Read(b []byte) (int, error) {
	tc.setDeadline()
	return tc.Conn.Read(b)
}

func (tc *timeoutConn) Write(b []byte) (int, error) {
	tc.setDeadline()
	return tc.Conn.Write(b)
}

// DialTimeout is like Dial, but the given timeout is used to set read/write
// deadlines on all reads/writes
func DialTimeout(network, addr string, timeout time.Duration) (Conn, error) {
	c, err := net.DialTimeout(network, addr, timeout)
	if err != nil {
		return nil, err
	}
	return NewConn(&timeoutConn{Conn: c, timeout: timeout}), nil
}
