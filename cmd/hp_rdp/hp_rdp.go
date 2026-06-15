//go:build js && wasm

package main

import (
	"context"
	"log"
	"syscall/js"

	"github.com/tale/headplane/internal/hp_ipn"
	"github.com/tale/headplane/internal/hp_rdp"
)

func main() {
	log.Printf("Loading WASM Headplane RDP module")

	factory := js.FuncOf(func(this js.Value, args []js.Value) any {
		if len(args) != 1 {
			log.Printf("Usage: create(config)")
			return nil
		}

		config, err := hp_ipn.ParseIPNConfig(args[0])
		if err != nil {
			log.Printf("Error parsing IPN config: %v", err)
			return nil
		}

		callbacks := hp_ipn.ParseIPNCallbacks(args[0])

		ipn, err := hp_ipn.NewTsWasmIpn(config, callbacks)
		if err != nil {
			callbacks.OnError(err.Error())
			return nil
		}

		go func() {
			if err := ipn.Start(context.Background()); err != nil {
				callbacks.OnError(err.Error())
			}
		}()

		return map[string]any{
			"openSession": js.FuncOf(func(this js.Value, args []js.Value) any {
				if len(args) != 1 {
					log.Printf("Usage: openSession(config)")
					return nil
				}

				rdpConfig, err := hp_rdp.ParseRDPConfig(args[0])
				if err != nil {
					log.Printf("Error parsing RDP config: %v", err)
					return nil
				}

				var session *hp_rdp.RDPSession
				go func() {
					var err error
					session, err = hp_rdp.NewRDPSession(ipn, rdpConfig)
					if err != nil {
						log.Printf("RDP session error: %v", err)
						rdpConfig.OnError(err.Error())
					}
				}()

				return map[string]any{
					"sendKey": js.FuncOf(func(this js.Value, args []js.Value) any {
						if len(args) != 2 || session == nil {
							return nil
						}
						scancode := args[0].Int()
						down := args[1].Bool()
						if down {
							session.KeyDown(scancode)
						} else {
							session.KeyUp(scancode)
						}
						return nil
					}),

					"sendMouse": js.FuncOf(func(this js.Value, args []js.Value) any {
						if len(args) != 4 || session == nil {
							return nil
						}
						button := args[0].Int()
						x := args[1].Int()
						y := args[2].Int()
						down := args[3].Bool()
						if button < 0 {
							session.MouseMove(x, y)
						} else if down {
							session.MouseDown(button, x, y)
						} else {
							session.MouseUp(button, x, y)
						}
						return nil
					}),

					"close": js.FuncOf(func(this js.Value, args []js.Value) any {
						if session != nil {
							session.Close()
						}
						return nil
					}),
				}
			}),
		}
	})

	resolve := js.Global().Get("__hp_rdp_resolve")
	if resolve.Type() != js.TypeFunction {
		log.Printf("__hp_rdp_resolve is not set, cannot initialize")
		return
	}

	resolve.Invoke(factory)
	js.Global().Delete("__hp_rdp_resolve")

	log.Printf("WASM Headplane RDP module loaded successfully")
	<-make(chan bool)
}
