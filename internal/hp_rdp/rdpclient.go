//go:build js && wasm

// rdpclient.go is a modified copy of github.com/tomatome/grdp/client/rdp.go.
// The key differences: Login accepts a pre-established net.Conn (from the
// Tailscale dialer) instead of calling net.DialTimeout internally, and the
// plugin/channel import is removed (CGo, incompatible with WASM).
package hp_rdp

import (
	"fmt"
	"net"
	"strings"

	"github.com/tomatome/grdp/core"
	"github.com/tomatome/grdp/protocol/nla"
	"github.com/tomatome/grdp/protocol/pdu"
	"github.com/tomatome/grdp/protocol/sec"
	"github.com/tomatome/grdp/protocol/t125"
	"github.com/tomatome/grdp/protocol/tpkt"
	"github.com/tomatome/grdp/protocol/x224"
)

type rdpClient struct {
	tpkt *tpkt.TPKT
	x224 *x224.X224
	mcs  *t125.MCSClient
	sec  *sec.Client
	pdu  *pdu.Client
}

// loginWithConn initialises the full RDP protocol stack on an already-connected
// net.Conn and completes the X.224 handshake. Callers must then read events
// via the pdu callbacks (onBitmap, onReady, onClose).
// bppToHighColor converts a bits-per-pixel value to the grdp HighColor constant.
func bppToHighColor(bpp int) uint16 {
	switch bpp {
	case 16:
		return 0x0010 // HIGH_COLOR_16BPP
	default:
		return 0x0018 // HIGH_COLOR_24BPP
	}
}

func loginWithConn(conn net.Conn, domain, user, password string, width, height, colorDepth int) (*rdpClient, error) {
	c := &rdpClient{}
	c.tpkt = tpkt.New(core.NewSocketLayer(conn), nla.NewNTLMv2(domain, user, password))
	c.x224 = x224.New(c.tpkt)
	c.mcs = t125.NewMCSClient(c.x224)
	c.sec = sec.NewClient(c.mcs)
	c.pdu = pdu.NewClient(c.sec)

	c.mcs.SetClientCoreData(uint16(width), uint16(height))
	c.mcs.SetColorDepth(bppToHighColor(colorDepth))
	c.sec.SetUser(user)
	c.sec.SetPwd(password)
	c.sec.SetDomain(domain)
	c.tpkt.SetFastPathListener(c.sec)
	c.sec.SetFastPathListener(c.pdu)
	c.sec.SetChannelSender(c.mcs)

	if err := c.x224.Connect(); err != nil {
		return nil, fmt.Errorf("x224 connect: %w", err)
	}
	return c, nil
}

func (c *rdpClient) onBitmap(f func([]pdu.BitmapData)) {
	c.pdu.On("update", func(data interface{}) {
		bitmaps, ok := data.([]pdu.BitmapData)
		if !ok {
			return
		}
		f(bitmaps)
	})
}

func (c *rdpClient) onReady(f func()) {
	c.pdu.On("ready", f)
}

func (c *rdpClient) onClose(f func()) {
	c.pdu.On("close", f)
}

func (c *rdpClient) close() {
	if c != nil && c.tpkt != nil {
		c.tpkt.Close()
	}
}

func (c *rdpClient) keyDown(scancode int) {
	p := &pdu.ScancodeKeyEvent{}
	if scancode&0x100 != 0 {
		p.KeyboardFlags |= pdu.KBDFLAGS_EXTENDED
	}
	p.KeyCode = uint16(scancode & 0xFF)
	c.pdu.SendInputEvents(pdu.INPUT_EVENT_SCANCODE, []pdu.InputEventsInterface{p})
}

func (c *rdpClient) keyUp(scancode int) {
	p := &pdu.ScancodeKeyEvent{}
	p.KeyboardFlags |= pdu.KBDFLAGS_RELEASE
	if scancode&0x100 != 0 {
		p.KeyboardFlags |= pdu.KBDFLAGS_EXTENDED
	}
	p.KeyCode = uint16(scancode & 0xFF)
	c.pdu.SendInputEvents(pdu.INPUT_EVENT_SCANCODE, []pdu.InputEventsInterface{p})
}

func (c *rdpClient) mouseMove(x, y int) {
	p := &pdu.PointerEvent{}
	p.PointerFlags |= pdu.PTRFLAGS_MOVE
	p.XPos = uint16(x)
	p.YPos = uint16(y)
	c.pdu.SendInputEvents(pdu.INPUT_EVENT_MOUSE, []pdu.InputEventsInterface{p})
}

func (c *rdpClient) mouseDown(button, x, y int) {
	p := &pdu.PointerEvent{}
	p.PointerFlags |= pdu.PTRFLAGS_DOWN
	switch button {
	case 0:
		p.PointerFlags |= pdu.PTRFLAGS_BUTTON1
	case 2:
		p.PointerFlags |= pdu.PTRFLAGS_BUTTON2
	case 1:
		p.PointerFlags |= pdu.PTRFLAGS_BUTTON3
	}
	p.XPos = uint16(x)
	p.YPos = uint16(y)
	c.pdu.SendInputEvents(pdu.INPUT_EVENT_MOUSE, []pdu.InputEventsInterface{p})
}

func (c *rdpClient) mouseUp(button, x, y int) {
	p := &pdu.PointerEvent{}
	switch button {
	case 0:
		p.PointerFlags |= pdu.PTRFLAGS_BUTTON1
	case 2:
		p.PointerFlags |= pdu.PTRFLAGS_BUTTON2
	case 1:
		p.PointerFlags |= pdu.PTRFLAGS_BUTTON3
	}
	p.XPos = uint16(x)
	p.YPos = uint16(y)
	c.pdu.SendInputEvents(pdu.INPUT_EVENT_MOUSE, []pdu.InputEventsInterface{p})
}

func splitUser(user string) (domain, uname string) {
	if idx := strings.Index(user, "\\"); idx >= 0 {
		return user[:idx], user[idx+1:]
	}
	if idx := strings.Index(user, "/"); idx >= 0 {
		return user[:idx], user[idx+1:]
	}
	return "", user
}

// bitmapToRGBA converts raw grdp bitmap data (BGR565 / BGR888 / BGRX) to RGBA.
func bitmapToRGBA(data []byte, bpp, width, height int) []byte {
	size := width * height * 4
	rgba := make([]byte, size)
	switch bpp {
	case 2: // 16bpp BGR565 — expand each channel to full 8 bits
		for i := 0; i < width*height && (i+1)*2 <= len(data); i++ {
			pixel := uint16(data[i*2]) | uint16(data[i*2+1])<<8
			r5 := (pixel >> 11) & 0x1F
			g6 := (pixel >> 5) & 0x3F
			b5 := pixel & 0x1F
			rgba[i*4] = uint8((r5 << 3) | (r5 >> 2))
			rgba[i*4+1] = uint8((g6 << 2) | (g6 >> 4))
			rgba[i*4+2] = uint8((b5 << 3) | (b5 >> 2))
			rgba[i*4+3] = 255
		}
	case 3: // 24bpp BGR
		for i := 0; i < width*height && (i+1)*3 <= len(data); i++ {
			rgba[i*4] = data[i*3+2]
			rgba[i*4+1] = data[i*3+1]
			rgba[i*4+2] = data[i*3]
			rgba[i*4+3] = 255
		}
	default: // 32bpp BGRX / BGRA
		for i := 0; i < width*height && (i+1)*4 <= len(data); i++ {
			rgba[i*4] = data[i*4+2]
			rgba[i*4+1] = data[i*4+1]
			rgba[i*4+2] = data[i*4]
			rgba[i*4+3] = 255
		}
	}
	return rgba
}
