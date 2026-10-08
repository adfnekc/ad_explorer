package httpx

import (
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/adfnekc/ad_explorer/internal/session"
	"github.com/adfnekc/ad_explorer/internal/wsframe"
)

// WebSocket timings and limits.
const (
	// writeWait bounds a single write, so one stalled client cannot pin a
	// goroutine forever.
	writeWait = 15 * time.Second
	// pongWait is how long the connection may be silent before it is considered
	// dead. The client sends an application-level ping about once a second,
	// which also counts.
	pongWait = 60 * time.Second
	// pingPeriod is how often the server sends a protocol-level ping. It must
	// be shorter than pongWait or a healthy connection would be reaped.
	pingPeriod = 25 * time.Second
	// maxClientMessage caps a control message from the browser. The largest
	// legitimate one is a paste of a few thousand characters.
	maxClientMessage = 256 << 10
)

// Application close codes, in the range the protocol reserves for applications.
const (
	closeAlreadyAttached = 4001
	closeSessionGone     = 4002
	closeBadMessage      = 4003
)

// handleStream upgrades to a WebSocket and hands it to the session.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sess, ok := s.cfg.Sessions.Get(id)
	if !ok {
		writeError(w, http.StatusNotFound, wsframe.ErrCodeSessionGone, "no such session")
		return
	}

	upgrader := websocket.Upgrader{
		HandshakeTimeout: 10 * time.Second,
		ReadBufferSize:   4096,
		// The buffer has to hold an encoded region, and a full frame is the
		// largest thing sent, so it is sized for a few of those.
		WriteBufferSize: 64 << 10,
		CheckOrigin:     func(r *http.Request) bool { return sameOrigin(r) },
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade has already written the failure response.
		return
	}

	transport := newWSTransport(conn)
	if err := sess.Attach(transport); err != nil {
		// The socket exists now, so the client can be told why rather than just
		// seeing it close. One session is one tab: two viewers would fight over
		// the pointer and the clipboard, and a reconnect that raced the old
		// socket's disconnect is told to try again rather than being mistaken
		// for a second viewer.
		if errors.Is(err, session.ErrAlreadyAttached) {
			transport.closeWith(closeAlreadyAttached, "this session already has a browser attached")
			return
		}
		transport.closeWith(closeSessionGone, "the session is no longer available")
		return
	}
}

// wsTransport adapts a gorilla connection to the session's Transport.
type wsTransport struct {
	conn *websocket.Conn
	once sync.Once
	done chan struct{}
}

func newWSTransport(conn *websocket.Conn) *wsTransport {
	conn.SetReadLimit(maxClientMessage)
	_ = conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongWait))
	})

	t := &wsTransport{conn: conn, done: make(chan struct{})}
	go t.pingLoop()
	return t
}

// pingLoop keeps the connection warm so an idle session is not reaped by a
// middlebox, and so a dead peer is noticed at all.
func (t *wsTransport) pingLoop() {
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			// WriteControl is safe to call alongside a write in progress, so
			// this needs no coordination with the session's writer.
			if err := t.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeWait)); err != nil {
				return
			}
		case <-t.done:
			return
		}
	}
}

// SendControl writes one JSON control message.
func (t *wsTransport) SendControl(c wsframe.Control) error {
	payload, err := wsframe.EncodeControl(c)
	if err != nil {
		return err
	}
	_ = t.conn.SetWriteDeadline(time.Now().Add(writeWait))
	return t.conn.WriteMessage(websocket.TextMessage, payload)
}

// SendFrames writes one binary message per frame.
//
// One frame per message rather than a concatenation: the message boundary then
// carries the framing, so the client can hand each message straight to a parser
// instead of tracking how far it got through a buffer.
func (t *wsTransport) SendFrames(frames []wsframe.Frame) error {
	_ = t.conn.SetWriteDeadline(time.Now().Add(writeWait))
	for _, f := range frames {
		buf, err := f.AppendTo(nil)
		if err != nil {
			return err
		}
		if err := t.conn.WriteMessage(websocket.BinaryMessage, buf); err != nil {
			return err
		}
	}
	return nil
}

// Receive reads the next message from the client.
//
// Text is a control frame and binary is a range of a file the client offered.
// Anything else is a protocol mistake and ends the connection rather than being
// ignored, because a client whose message was dropped would wait for an answer
// that is never coming.
func (t *wsTransport) Receive() (session.Message, error) {
	for {
		kind, payload, err := t.conn.ReadMessage()
		if err != nil {
			return nil, err
		}
		_ = t.conn.SetReadDeadline(time.Now().Add(pongWait))

		// A message that cannot be decoded is answered with a close frame
		// saying so. Dropping the connection without one leaves the client
		// staring at an abnormal closure with nothing to act on.
		switch kind {
		case websocket.TextMessage:
			c, err := wsframe.DecodeControl(payload)
			if err != nil {
				t.closeWith(closeBadMessage, "the client sent a control message this gateway cannot read")
				return nil, err
			}
			return session.ControlMessage{Control: c}, nil

		case websocket.BinaryMessage:
			frame, err := wsframe.ParseFrame(payload)
			if err != nil {
				t.closeWith(closeBadMessage, "the client sent a binary message that is not a frame")
				return nil, err
			}
			token, offset, data, err := wsframe.ParseFileChunk(frame)
			if err != nil {
				t.closeWith(closeBadMessage, "the client sent a frame that is not a file chunk")
				return nil, err
			}
			return session.ChunkMessage{Token: token, Offset: offset, Data: data}, nil

		default:
			t.closeWith(closeBadMessage, "the client sent a message type this protocol does not use")
			return nil, errBadClientMessage
		}
	}
}

// Close releases the connection. It is safe to call more than once.
func (t *wsTransport) Close() error {
	var err error
	t.once.Do(func() {
		close(t.done)
		err = t.conn.Close()
	})
	return err
}

// closeWith sends a close frame carrying a reason, then closes.
func (t *wsTransport) closeWith(code int, reason string) {
	_ = t.conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(code, reason),
		time.Now().Add(writeWait),
	)
	_ = t.Close()
}

// errBadClientMessage is returned when a client sends something the protocol
// does not allow. It ends the session through the normal path.
var errBadClientMessage = errors.New("httpx: the client sent a binary message")
