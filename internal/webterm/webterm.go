// Package webterm bridges a browser xterm.js terminal to a local PTY running an
// interactive shell (e.g. `lxc exec <name> -- bash`) over a WebSocket.
//
// Protocol:
//   - client -> server binary frame: raw stdin bytes
//   - client -> server text frame:   JSON control message, e.g. {"type":"resize","cols":80,"rows":24}
//   - server -> client binary frame: raw stdout/stderr bytes
package webterm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// CommandFactory builds the *exec.Cmd to attach to a PTY for a given container.
type CommandFactory func(ctx context.Context, container string) *exec.Cmd

// Handler upgrades WebSocket requests and pipes them to a PTY-backed command.
type Handler struct {
	newCommand CommandFactory
	upgrader   websocket.Upgrader
}

// NewHandler returns a Handler that runs commands built by factory.
func NewHandler(factory CommandFactory) *Handler {
	return &Handler{
		newCommand: factory,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
			// Same-origin only: the auth middleware already gates this route and
			// the app is single-origin, so reject cross-origin upgrades.
			CheckOrigin: sameOrigin,
		},
	}
}

type control struct {
	Type string `json:"type"`
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// ServeWS handles a terminal WebSocket for the named container.
func (h *Handler) ServeWS(w http.ResponseWriter, r *http.Request, container string) {
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote an error response.
		return
	}
	defer conn.Close()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	cmd := h.newCommand(ctx, container)
	pty, err := startPTY(cmd)
	if err != nil {
		_ = conn.WriteMessage(websocket.TextMessage, []byte("\r\n[failed to start terminal: "+err.Error()+"]\r\n"))
		return
	}
	defer pty.Close()

	var writeMu sync.Mutex
	writeBinary := func(b []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return conn.WriteMessage(websocket.BinaryMessage, b)
	}

	// PTY -> WebSocket
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := pty.Read(buf)
			if n > 0 {
				if werr := writeBinary(buf[:n]); werr != nil {
					cancel()
					return
				}
			}
			if err != nil {
				cancel()
				return
			}
		}
	}()

	// Close the socket when the command exits.
	go func() {
		<-ctx.Done()
		writeMu.Lock()
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, "session ended"),
			time.Now().Add(time.Second))
		writeMu.Unlock()
		_ = conn.Close()
	}()

	// WebSocket -> PTY
	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		switch mt {
		case websocket.BinaryMessage:
			if _, err := pty.Write(data); err != nil {
				return
			}
		case websocket.TextMessage:
			var ctl control
			if json.Unmarshal(data, &ctl) == nil && ctl.Type == "resize" {
				_ = pty.Resize(ctl.Rows, ctl.Cols)
				continue
			}
			// Unknown text frame: treat as stdin.
			if _, err := pty.Write(data); err != nil {
				return
			}
		}
	}
}

func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // non-browser client
	}
	return originHost(origin) == r.Host
}

func originHost(origin string) string {
	if i := strings.Index(origin, "://"); i >= 0 {
		origin = origin[i+3:]
	}
	return origin
}

// ptySession is a started PTY-backed process.
type ptySession interface {
	io.ReadWriteCloser
	Resize(rows, cols uint16) error
}
