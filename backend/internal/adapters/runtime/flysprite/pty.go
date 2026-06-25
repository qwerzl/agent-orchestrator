package flysprite

import (
	"context"
	"io"
	"sync"

	sprites "github.com/superfly/sprites-go"
)

// spritePTY adapts a sprite TTY exec to terminal.PTYProcess (io.ReadWriteCloser
// + Resize). Read drains the attached pane's output; Write delivers keystrokes;
// Close detaches THIS client by cancelling its exec context — it never touches
// the in-sprite zellij session, which the zellij server keeps alive for other
// clients (mirroring the local "PTY per client, session is the multiplexer"
// model).
type spritePTY struct {
	cmd       *sprites.Cmd
	stdin     io.WriteCloser
	stdout    io.ReadCloser
	cancel    context.CancelFunc
	closeOnce sync.Once
}

func (p *spritePTY) Read(b []byte) (int, error)  { return p.stdout.Read(b) }
func (p *spritePTY) Write(b []byte) (int, error) { return p.stdin.Write(b) }

func (p *spritePTY) Resize(rows, cols uint16) error { return p.cmd.Resize(rows, cols) }

func (p *spritePTY) Close() error {
	p.closeOnce.Do(func() {
		_ = p.stdin.Close()
		if p.cancel != nil {
			p.cancel()
		}
	})
	return nil
}
