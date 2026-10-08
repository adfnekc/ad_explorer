// Package session owns the live remote desktop sessions: it holds the grdp
// connection, drives the encoder, and translates between the browser's control
// messages and the remote's input.
//
// It knows nothing about HTTP or WebSocket. A session speaks to a Transport,
// which is an interface, so the whole event and input path can be tested with a
// recorder instead of a socket.
package session

import (
	"errors"
	"time"

	"github.com/adfnekc/ad_explorer/internal/wsframe"
)

// Transport is the link to one browser tab.
//
// Implementations do not need to be safe for concurrent use: a Session
// serialises every call to SendControl and SendFrames, and only ever calls
// ReceiveControl from one goroutine.
type Transport interface {
	// SendControl writes one JSON control message.
	SendControl(wsframe.Control) error
	// SendFrames writes encoded frames, in order.
	SendFrames([]wsframe.Frame) error
	// Receive blocks until the client sends something. It returns an error once
	// the connection is gone, which ends the session.
	//
	// One call for both shapes rather than two: two blocking reads would need
	// two goroutines and would deliver a control frame and the chunk that
	// belongs to it out of order.
	Receive() (Message, error)
	// Close releases the connection. It must be safe to call more than once.
	Close() error
}

// Message is one thing the client sent.
type Message interface{ message() }

// ControlMessage is a JSON control frame.
type ControlMessage struct {
	Control wsframe.Control
}

func (ControlMessage) message() {}

// ChunkMessage is a range of an offered file, answering a FilesNeed.
//
// The token is what ties it to the request: several ranges of several files may
// be in flight, and the offset says where in the file the bytes belong.
type ChunkMessage struct {
	Token  uint32
	Offset uint32
	Data   []byte
}

func (ChunkMessage) message() {}

// Errors returned by this package.
var (
	// ErrLimit means the gateway is already running as many sessions as it is
	// allowed to. The HTTP layer maps it to 429.
	ErrLimit = errors.New("session: concurrent session limit reached")
	// ErrNotFound means no session has that id. The HTTP layer maps it to 404.
	ErrNotFound = errors.New("session: no such session")
	// ErrShuttingDown means the gateway is stopping and will not accept new
	// sessions.
	ErrShuttingDown = errors.New("session: shutting down")
	// ErrAlreadyAttached means this session already has a browser on it. One
	// session is one tab; a second viewer would fight over the cursor.
	ErrAlreadyAttached = errors.New("session: already attached")
	// ErrClosed means the session has ended.
	ErrClosed = errors.New("session: closed")
)

// Timings. They are variables so the tests can shorten them without waiting.
var (
	// statsInterval is how often the HUD is refreshed.
	statsInterval = time.Second
	// idleCheckInterval is how often the idle deadline is tested. It only has
	// to be fine relative to the timeout, which is minutes.
	idleCheckInterval = 15 * time.Second
)

// Info describes a session to the API, without exposing anything secret.
type Info struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Host      string    `json:"host"`
	Width     int       `json:"width"`
	Height    int       `json:"height"`
	StartedAt time.Time `json:"startedAt"`
	Attached  bool      `json:"attached"`
}
