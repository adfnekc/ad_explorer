package httpx

import (
	"encoding/json"
	"fmt"
	"image"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/adfnekc/ad_explorer/internal/session"
	"github.com/adfnekc/ad_explorer/internal/wsframe"
)

// wsURL turns the test server's http URL into the ws one.
func wsURL(srv *httptest.Server) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// serveUpgrade runs handler against a real WebSocket connection, so the
// transport is exercised over a socket rather than against a mock.
func serveUpgrade(t *testing.T, handler func(*wsTransport)) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		transport := newWSTransport(conn)
		defer transport.Close()
		handler(transport)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func dialWS(t *testing.T, srv *httptest.Server) *websocket.Conn {
	t.Helper()
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL(srv), nil)
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("dial: %v (status %d)", err, status)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// drainErr reports what the server side made of the exchange, for a failure
// message that says more than "the socket closed".
func drainErr(ch <-chan error) error {
	select {
	case err := <-ch:
		return err
	case <-time.After(time.Second):
		return nil
	}
}

func TestWSTransportCarriesControlAndFrames(t *testing.T) {
	serverErr := make(chan error, 1)

	srv := serveUpgrade(t, func(tr *wsTransport) {
		// In: a control message from the browser.
		msg, err := tr.Receive()
		if err != nil {
			serverErr <- err
			return
		}
		ctrl, ok := msg.(session.ControlMessage)
		if !ok {
			serverErr <- fmt.Errorf("expected a control message, got %T", msg)
			return
		}
		key, ok := ctrl.Control.(*wsframe.InputKey)
		if !ok {
			serverErr <- fmt.Errorf("expected InputKey, got %s", ctrl.Control.ControlType())
			return
		}
		if key.Code != "KeyA" || !key.Down {
			serverErr <- fmt.Errorf("got %+v, want KeyA down", key)
			return
		}

		// Out: a control message, then two frames.
		if err := tr.SendControl(wsframe.Pong{T: 42}); err != nil {
			serverErr <- err
			return
		}
		frames := []wsframe.Frame{
			{
				Type: wsframe.TypeImage, Format: wsframe.FormatJPEG, Flags: wsframe.FlagKeyframe,
				Rect: image.Rect(0, 0, 16, 8), Seq: 1, Payload: []byte("first"),
			},
			{
				Type: wsframe.TypeCursor, Format: wsframe.FormatPNG,
				Rect: image.Rect(2, 3, 26, 27), Seq: 2, Payload: []byte("second"),
			},
		}
		if err := tr.SendFrames(frames); err != nil {
			serverErr <- err
			return
		}
		// Wait for the client to say it has read everything. Returning here
		// would run the deferred Close and cut the socket before the last bytes
		// left the kernel buffer, which the client sees as an abnormal closure.
		_, err = tr.Receive()
		serverErr <- err
	})

	conn := dialWS(t, srv)

	payload, err := wsframe.EncodeControl(wsframe.InputKey{Code: "KeyA", Down: true})
	if err != nil {
		t.Fatalf("EncodeControl: %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
		t.Fatalf("write: %v", err)
	}

	// The control message comes back as text.
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	kind, body, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read control: %v (server side: %v)", err, drainErr(serverErr))
	}
	if kind != websocket.TextMessage {
		t.Fatalf("control arrived as %d, want text", kind)
	}
	// DecodeControl only knows the client-to-server types, which is deliberate:
	// the gateway never decodes a message it sent. The reply is checked as JSON.
	var reply map[string]any
	if err := json.Unmarshal(body, &reply); err != nil {
		t.Fatalf("the reply is not JSON: %v (%s)", err, body)
	}
	if reply["type"] != "pong" {
		t.Errorf("reply type = %v, want pong", reply["type"])
	}
	if t42, ok := reply["t"].(float64); !ok || t42 != 42 {
		t.Errorf("reply t = %v, want 42", reply["t"])
	}

	// Each frame arrives as its own binary message, ready to parse.
	wantSeq := []uint32{1, 2}
	wantPayload := []string{"first", "second"}
	for i := range wantSeq {
		kind, body, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read frame %d: %v", i, err)
		}
		if kind != websocket.BinaryMessage {
			t.Fatalf("frame %d arrived as %d, want binary", i, kind)
		}
		f, err := wsframe.ParseFrame(body)
		if err != nil {
			t.Fatalf("ParseFrame %d: %v", i, err)
		}
		if f.Seq != wantSeq[i] || string(f.Payload) != wantPayload[i] {
			t.Errorf("frame %d = seq %d payload %q, want seq %d payload %q",
				i, f.Seq, f.Payload, wantSeq[i], wantPayload[i])
		}
	}

	// Tell the server the client is done, so it can close without racing.
	ack, err := wsframe.EncodeControl(wsframe.Ping{T: 1})
	if err != nil {
		t.Fatalf("EncodeControl: %v", err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, ack); err != nil {
		t.Fatalf("write ack: %v", err)
	}

	select {
	case err := <-serverErr:
		if err != nil {
			t.Fatalf("server side: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the server handler never finished")
	}
}

func TestWSTransportRefusesBinaryThatIsNotAFileChunk(t *testing.T) {
	// A binary message the protocol does not define is a mistake worth naming:
	// a client whose message was ignored would wait for an answer forever.
	cases := []struct {
		name    string
		payload []byte
	}{
		{"not a frame at all", []byte{1, 2, 3}},
		{"a valid frame that is not a file chunk", func() []byte {
			// An image frame parses, so this reaches the second check.
			b, _ := wsframe.Frame{
				Type: wsframe.TypeImage, Format: wsframe.FormatJPEG,
				Rect: image.Rect(0, 0, 1, 1), Payload: []byte{1, 2, 3, 4},
			}.AppendTo(nil)
			return b
		}()},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotErr := make(chan error, 1)
			srv := serveUpgrade(t, func(tr *wsTransport) {
				_, err := tr.Receive()
				gotErr <- err
			})

			conn := dialWS(t, srv)
			if err := conn.WriteMessage(websocket.BinaryMessage, tc.payload); err != nil {
				t.Fatalf("write: %v", err)
			}

			select {
			case err := <-gotErr:
				if err == nil {
					t.Fatal("the message was accepted")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("the server did not react")
			}

			// The client is told why, rather than just seeing the socket vanish.
			_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			for {
				_, _, err := conn.ReadMessage()
				if err == nil {
					continue
				}
				ce, ok := err.(*websocket.CloseError)
				if !ok {
					t.Fatalf("connection ended with %v, want a close frame", err)
				}
				if ce.Code != closeBadMessage {
					t.Errorf("close code = %d, want %d", ce.Code, closeBadMessage)
				}
				if !strings.Contains(ce.Text, "chunk") && !strings.Contains(ce.Text, "frame") {
					t.Errorf("close reason = %q, want it to say what was wrong", ce.Text)
				}
				return
			}
		})
	}
}

func TestWSTransportRejectsMalformedControl(t *testing.T) {
	gotErr := make(chan error, 1)

	srv := serveUpgrade(t, func(tr *wsTransport) {
		_, err := tr.Receive()
		gotErr <- err
	})

	conn := dialWS(t, srv)
	// Valid JSON, unknown type: the decoder must refuse rather than pass it up.
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"input.teleport"}`)); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case err := <-gotErr:
		if err == nil {
			t.Fatal("an unknown control type was accepted")
		}
		if !strings.Contains(err.Error(), "unknown control type") {
			t.Errorf("error = %v, want it to name the problem", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the server did not react")
	}
}

func TestWSTransportCloseIsIdempotent(t *testing.T) {
	released := make(chan struct{})

	srv := serveUpgrade(t, func(tr *wsTransport) {
		tr.Close()
		tr.Close()
		tr.Close()
		close(released)
	})

	conn := dialWS(t, srv)
	select {
	case <-released:
	case <-time.After(3 * time.Second):
		t.Fatal("Close blocked or panicked")
	}
	// A closed transport must report the failure rather than block forever.
	_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
	_ = conn.Close()
}
