//go:build js && wasm

package hp_rdp

import (
	"errors"
	"syscall/js"
)

type RDPConfig struct {
	IPAddress    string
	Username     string
	Password     string
	Domain       string
	Width        int
	Height       int
	ColorDepth   int // bits per pixel: 16 or 24 (default)
	OnUpdate     func(x, y, w, h int, pixels js.Value)
	OnConnect    func()
	OnDisconnect func()
	OnError      func(msg string)
}

func ParseRDPConfig(obj js.Value) (*RDPConfig, error) {
	if obj.IsUndefined() || obj.IsNull() {
		return nil, errors.New("rdp config cannot be undefined or null")
	}

	ipAddress := safeString("ipAddress", obj)
	username := safeString("username", obj)
	password := safeString("password", obj)
	if ipAddress == "" || username == "" || password == "" {
		return nil, errors.New("missing required fields: ipAddress, username, password")
	}

	width := safeInt("width", obj)
	height := safeInt("height", obj)
	if width <= 0 {
		width = 1280
	}
	if height <= 0 {
		height = 720
	}

	colorDepth := safeInt("colorDepth", obj)
	if colorDepth != 16 && colorDepth != 24 {
		colorDepth = 24
	}

	config := &RDPConfig{
		IPAddress:  ipAddress,
		Username:   username,
		Password:   password,
		Domain:     safeString("domain", obj),
		Width:      width,
		Height:     height,
		ColorDepth: colorDepth,
	}

	onUpdate := obj.Get("onUpdate")
	if onUpdate.IsUndefined() || onUpdate.IsNull() || onUpdate.Type() != js.TypeFunction {
		return nil, errors.New("`onUpdate` is required and must be a function")
	}
	config.OnUpdate = func(x, y, w, h int, pixels js.Value) {
		onUpdate.Invoke(x, y, w, h, pixels)
	}

	onConnect := obj.Get("onConnect")
	if onConnect.Type() == js.TypeFunction {
		config.OnConnect = func() { onConnect.Invoke() }
	} else {
		config.OnConnect = func() {}
	}

	onDisconnect := obj.Get("onDisconnect")
	if onDisconnect.Type() == js.TypeFunction {
		config.OnDisconnect = func() { onDisconnect.Invoke() }
	} else {
		config.OnDisconnect = func() {}
	}

	onError := obj.Get("onError")
	if onError.Type() == js.TypeFunction {
		config.OnError = func(msg string) { onError.Invoke(msg) }
	} else {
		config.OnError = func(string) {}
	}

	return config, nil
}

func safeString(key string, obj js.Value) string {
	if obj.IsUndefined() || obj.IsNull() {
		return ""
	}
	val := obj.Get(key)
	if val.IsUndefined() || val.IsNull() {
		return ""
	}
	return val.String()
}

func safeInt(key string, obj js.Value) int {
	if obj.IsUndefined() || obj.IsNull() {
		return 0
	}
	val := obj.Get(key)
	if val.IsUndefined() || val.IsNull() {
		return 0
	}
	return val.Int()
}
