package cli

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// detachByte is the local "detach" key (Ctrl-]). Pressing it leaves the attach
// without touching the remote session, which keeps running. Every other byte —
// including Ctrl-C — is forwarded to the agent.
const detachByte = 0x1d

// muxReadLimit bounds a single inbound mux frame. PTY output is chunked
// server-side, but base64 inflates it, so allow generous headroom.
const muxReadLimit = 16 << 20

// muxClientMsg / muxServerMsg mirror the wire protocol in internal/terminal
// (protocol.go). The CLI keeps its own copy so it need not import the daemon's
// terminal package.
type muxClientMsg struct {
	Ch   string `json:"ch"`
	ID   string `json:"id,omitempty"`
	Type string `json:"type"`
	Data string `json:"data,omitempty"`
	Cols uint16 `json:"cols,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
}

type muxServerMsg struct {
	Ch    string `json:"ch"`
	ID    string `json:"id,omitempty"`
	Type  string `json:"type"`
	Data  string `json:"data,omitempty"`
	Error string `json:"error,omitempty"`
}

// sessionHandleResponse is the subset of GET /api/v1/sessions/{id} the attach
// command needs: the opaque runtime handle that keys the mux terminal stream.
type sessionHandleResponse struct {
	TerminalHandleID string `json:"terminalHandleId"`
	Status           string `json:"status"`
}

func newAttachCommand(ctx *commandContext) *cobra.Command {
	return &cobra.Command{
		Use:   "attach <session>",
		Short: "Attach an interactive terminal to a running session",
		Long: "Attach your terminal to a running session's agent pane over the daemon's " +
			"terminal mux. Works against a local daemon or a remote one (set AO_DAEMON_URL " +
			"and AO_AUTH_TOKEN). Press Ctrl-] to detach without stopping the session.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return ctx.attach(cmd.Context(), args[0])
		},
	}
}

func (c *commandContext) attach(ctx context.Context, session string) error {
	session = strings.TrimSpace(session)
	if session == "" {
		return usageError{errors.New("usage: ao attach <session>")}
	}

	var sess sessionHandleResponse
	if err := c.getJSON(ctx, "sessions/"+url.PathEscape(session), &sess); err != nil {
		return err
	}
	if sess.TerminalHandleID == "" {
		return fmt.Errorf("session %s has no attachable terminal (status %s)", session, sess.Status)
	}

	target, err := c.resolveDaemonTarget()
	if err != nil {
		return err
	}

	stdinFd := int(os.Stdin.Fd())
	if !term.IsTerminal(stdinFd) {
		return errors.New("ao attach requires an interactive terminal")
	}

	dialOpts := &websocket.DialOptions{}
	if target.token != "" {
		dialOpts.HTTPHeader = http.Header{"Authorization": []string{"Bearer " + target.token}}
	}
	conn, _, err := websocket.Dial(ctx, target.wsURL("/mux"), dialOpts)
	if err != nil {
		return fmt.Errorf("connect to daemon mux: %w", err)
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "detach") }()
	conn.SetReadLimit(muxReadLimit)

	oldState, err := term.MakeRaw(stdinFd)
	if err != nil {
		return fmt.Errorf("enter raw terminal mode: %w", err)
	}
	defer func() { _ = term.Restore(stdinFd, oldState) }()

	fmt.Fprintf(c.deps.Err, "attached to %s — press Ctrl-] to detach\r\n", session)

	cols, rows := termSize(stdinFd)
	if err := wsjson.Write(ctx, conn, muxClientMsg{Ch: "terminal", Type: "open", ID: sess.TerminalHandleID, Cols: cols, Rows: rows}); err != nil {
		return fmt.Errorf("open terminal: %w", err)
	}

	// Cancel everything when any leg finishes (detach, server exit, error).
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	errCh := make(chan error, 3)

	go c.pumpStdin(runCtx, conn, sess.TerminalHandleID, cancel, errCh)
	go c.pollResize(runCtx, conn, sess.TerminalHandleID, stdinFd, cols, rows)
	go pingLoop(runCtx, conn)

	// Main loop: server frames → stdout.
	for {
		var msg muxServerMsg
		if err := wsjson.Read(runCtx, conn, &msg); err != nil {
			if runCtx.Err() != nil {
				return drain(errCh)
			}
			return fmt.Errorf("read from daemon: %w", err)
		}
		switch {
		case msg.Ch == "terminal" && msg.Type == "data":
			b, derr := base64.StdEncoding.DecodeString(msg.Data)
			if derr != nil {
				continue
			}
			_, _ = os.Stdout.Write(b)
		case msg.Ch == "terminal" && msg.Type == "exited":
			fmt.Fprintf(c.deps.Err, "\r\nsession terminal exited\r\n")
			return drain(errCh)
		case msg.Ch == "terminal" && msg.Type == "error":
			return fmt.Errorf("daemon terminal error: %s", msg.Error)
		}
	}
}

// pumpStdin forwards local keystrokes to the agent pane, honoring the detach key.
func (c *commandContext) pumpStdin(ctx context.Context, conn *websocket.Conn, handle string, cancel context.CancelFunc, errCh chan<- error) {
	buf := make([]byte, 4096)
	for {
		n, err := os.Stdin.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if i := indexByte(chunk, detachByte); i >= 0 {
				if i > 0 {
					_ = wsjson.Write(ctx, conn, dataMsg(handle, chunk[:i]))
				}
				cancel()
				errCh <- nil
				return
			}
			if werr := wsjson.Write(ctx, conn, dataMsg(handle, chunk)); werr != nil {
				errCh <- werr
				cancel()
				return
			}
		}
		if err != nil {
			errCh <- nil
			cancel()
			return
		}
	}
}

// pollResize watches the local terminal size and pushes a resize frame when it
// changes. Polling (rather than SIGWINCH) keeps the command cross-platform.
func (c *commandContext) pollResize(ctx context.Context, conn *websocket.Conn, handle string, fd int, cols, rows uint16) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			nc, nr := termSize(fd)
			if nc != cols || nr != rows {
				cols, rows = nc, nr
				_ = wsjson.Write(ctx, conn, muxClientMsg{Ch: "terminal", Type: "resize", ID: handle, Cols: cols, Rows: rows})
			}
		}
	}
}

// pingLoop keeps the mux connection alive through idle-timeout proxies (e.g. the
// Fly edge drops idle data connections after ~30s).
func pingLoop(ctx context.Context, conn *websocket.Conn) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := wsjson.Write(ctx, conn, muxClientMsg{Ch: "system", Type: "ping"}); err != nil {
				return
			}
		}
	}
}

func dataMsg(handle string, b []byte) muxClientMsg {
	return muxClientMsg{Ch: "terminal", Type: "data", ID: handle, Data: base64.StdEncoding.EncodeToString(b)}
}

func termSize(fd int) (cols, rows uint16) {
	w, h, err := term.GetSize(fd)
	if err != nil || w <= 0 || h <= 0 {
		return 0, 0
	}
	return uint16(w), uint16(h)
}

func indexByte(b []byte, target byte) int {
	for i, x := range b {
		if x == target {
			return i
		}
	}
	return -1
}

// drain returns the first non-nil error parked by a goroutine, or nil.
func drain(errCh chan error) error {
	for {
		select {
		case err := <-errCh:
			if err != nil {
				return err
			}
		default:
			return nil
		}
	}
}
