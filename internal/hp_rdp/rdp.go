//go:build js && wasm

package hp_rdp

import (
	"context"
	"fmt"
	"net"
	"syscall/js"

	"github.com/tomatome/grdp/core"
	"github.com/tomatome/grdp/glog"
	"github.com/tomatome/grdp/protocol/pdu"
)

// Dialer is implemented by TsWasmIpn.
type Dialer interface {
	Dial(ctx context.Context, network, addr string) (net.Conn, error)
}

type RDPSession struct {
	client *rdpClient
	cancel context.CancelFunc
}

func init() {
	// grdp's glog panics if any log call runs before a logger is set.
	// Suppress all logging by setting level to NONE.
	glog.SetLevel(glog.NONE)
}

func NewRDPSession(ipn Dialer, cfg *RDPConfig) (*RDPSession, error) {
	ctx, cancel := context.WithCancel(context.Background())

	addr := net.JoinHostPort(cfg.IPAddress, "3389")
	conn, err := ipn.Dial(ctx, "tcp", addr)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}

	domain, uname := splitUser(cfg.Username)
	if cfg.Domain != "" {
		domain = cfg.Domain
	}

	client, err := loginWithConn(conn, domain, uname, cfg.Password, cfg.Width, cfg.Height, cfg.ColorDepth)
	if err != nil {
		cancel()
		conn.Close()
		return nil, fmt.Errorf("rdp login: %w", err)
	}

	s := &RDPSession{client: client, cancel: cancel}

	client.onReady(func() {
		cfg.OnConnect()
	})

	client.onClose(func() {
		cfg.OnDisconnect()
	})

	client.onBitmap(func(bitmaps []pdu.BitmapData) {
		for _, bm := range bitmaps {
			x := int(bm.DestLeft)
			y := int(bm.DestTop)
			bpp := int(bm.BitsPerPixel) / 8

			// pw/ph are the pixel dimensions of the decoded RGBA data.
			// For compressed tiles, Decompress produces bm.Width×bm.Height pixels.
			// For uncompressed tiles, the data matches the dest rect exactly.
			var raw []byte
			var pw, ph int
			if bm.IsCompress() {
				pw, ph = int(bm.Width), int(bm.Height)
				raw = core.Decompress(bm.BitmapDataStream, pw, ph, bpp)
			} else {
				pw = int(bm.DestRight-bm.DestLeft) + 1
				ph = int(bm.DestBottom-bm.DestTop) + 1
				raw = bm.BitmapDataStream
			}

			rgba := bitmapToRGBA(raw, bpp, pw, ph)

			jsArr := js.Global().Get("Uint8ClampedArray").New(len(rgba))
			js.CopyBytesToJS(jsArr, rgba)
			cfg.OnUpdate(x, y, pw, ph, jsArr)
		}
	})

	return s, nil
}

func (s *RDPSession) Close() {
	s.cancel()
	s.client.close()
}

func (s *RDPSession) KeyDown(scancode int) { s.client.keyDown(scancode) }
func (s *RDPSession) KeyUp(scancode int)   { s.client.keyUp(scancode) }

func (s *RDPSession) MouseMove(x, y int)            { s.client.mouseMove(x, y) }
func (s *RDPSession) MouseDown(button, x, y int)    { s.client.mouseDown(button, x, y) }
func (s *RDPSession) MouseUp(button, x, y int)      { s.client.mouseUp(button, x, y) }
