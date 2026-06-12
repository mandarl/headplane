// ws_bridge bridges WebSocket ts2021 connections to Headscale's HTTP/1.1
// POST+Upgrade endpoint. Needed because Headscale 0.29+ only accepts POST for
// ts2021, but the Tailscale WASM client (browser) must use WebSocket.
package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"

	"github.com/coder/websocket"
	"tailscale.com/net/wsconn"
)

var (
	listenAddr    = flag.String("listen", "localhost:8083", "bridge listen address")
	headscaleAddr = flag.String("headscale", "localhost:8080", "headscale address")
)

func main() {
	flag.Parse()
	log.Printf("ws-bridge: %s → %s", *listenAddr, *headscaleAddr)
	log.Fatal(http.ListenAndServe(*listenAddr, http.HandlerFunc(handle)))
}

func handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/ts2021" && r.Header.Get("Upgrade") == "websocket" {
		bridgeWS(w, r)
		return
	}
	http.Error(w, "not found", http.StatusNotFound)
}

func bridgeWS(w http.ResponseWriter, r *http.Request) {
	initB64 := r.URL.Query().Get("X-Tailscale-Handshake")
	if initB64 == "" {
		http.Error(w, "missing X-Tailscale-Handshake", http.StatusBadRequest)
		return
	}

	wsConn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:    []string{"tailscale-control-protocol"},
		OriginPatterns:  []string{"*"},
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		log.Printf("ws accept: %v", err)
		return
	}
	defer wsConn.CloseNow()

	if wsConn.Subprotocol() != "tailscale-control-protocol" {
		wsConn.Close(websocket.StatusPolicyViolation, "wrong subprotocol")
		return
	}

	tcp, err := net.Dial("tcp", *headscaleAddr)
	if err != nil {
		log.Printf("dial headscale: %v", err)
		wsConn.Close(websocket.StatusInternalError, "upstream unavailable")
		return
	}
	defer tcp.Close()

	// Send HTTP POST upgrade to Headscale (POST is the only method 0.29 allows)
	_, err = fmt.Fprintf(tcp,
		"POST /ts2021 HTTP/1.1\r\nHost: %s\r\nUpgrade: tailscale-control-protocol\r\nConnection: Upgrade\r\nX-Tailscale-Handshake: %s\r\nContent-Length: 0\r\n\r\n",
		*headscaleAddr, initB64)
	if err != nil {
		log.Printf("write upgrade: %v", err)
		return
	}

	// Read 101 Switching Protocols from Headscale
	br := bufio.NewReader(tcp)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		log.Printf("read response: %v", err)
		return
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		log.Printf("headscale returned %d, expected 101", resp.StatusCode)
		return
	}

	// Any bytes already buffered by bufio.Reader must be prepended
	var headscaleRead io.Reader = tcp
	if br.Buffered() > 0 {
		buf := make([]byte, br.Buffered())
		br.Read(buf)
		headscaleRead = io.MultiReader(bytes.NewReader(buf), tcp)
	}

	// Wrap WebSocket as a net.Conn stream (same abstraction Tailscale uses)
	ctx := context.Background()
	browserConn := wsconn.NetConn(ctx, wsConn, websocket.MessageBinary, r.RemoteAddr)

	// Bidirectional pipe: browser ↔ headscale
	done := make(chan struct{}, 2)
	go func() {
		io.Copy(tcp, browserConn)
		done <- struct{}{}
	}()
	go func() {
		io.Copy(browserConn, headscaleRead)
		done <- struct{}{}
	}()
	<-done
	log.Printf("bridge closed for %s", r.RemoteAddr)
}
