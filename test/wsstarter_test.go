package test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/golang-acexy/starter-websocket/wsstarter"
)

func TestServerStartHandleAndStop(t *testing.T) {
	starter := &wsstarter.WebsocketStarter{
		Config: wsstarter.WebsocketConfig{
			ListenAddress: "127.0.0.1:0",
			Routers: []*wsstarter.Router{
				{
					Path: "/ws",
					Handler: func(message wsstarter.Message, conn *wsstarter.Conn) {
						if err := conn.SendMessage(message); err != nil {
							t.Errorf("server send failed: %v", err)
						}
					},
				},
			},
		},
	}
	instance, err := starter.Start()
	if err != nil {
		t.Fatalf("start failed: %v", err)
	}
	server := instance.(*http.Server)
	_, port, err := net.SplitHostPort(server.Addr)
	if err != nil {
		t.Fatalf("split server address failed: %v", err)
	}
	conn, _, err := websocket.Dial(context.Background(), "ws://127.0.0.1:"+port+"/ws", nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	defer conn.CloseNow()
	if err = conn.Write(context.Background(), websocket.MessageText, []byte("hello")); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	messageType, data, err := conn.Read(context.Background())
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if messageType != websocket.MessageText || string(data) != "hello" {
		t.Fatalf("unexpected message: type=%v data=%q", messageType, data)
	}
	gracefully, stopped, err := starter.Stop(time.Second)
	if err != nil || !gracefully || !stopped {
		t.Fatalf("unexpected stop result: gracefully=%v stopped=%v err=%v", gracefully, stopped, err)
	}
	if wsstarter.RawWebsocketServer() != nil {
		t.Fatal("raw server should be cleared after stop")
	}
}

func TestServerValidatesRouters(t *testing.T) {
	tests := []struct {
		name     string
		routers  []*wsstarter.Router
		expected error
	}{
		{name: "missing routers", expected: wsstarter.ErrMissRouters},
		{name: "nil router", routers: []*wsstarter.Router{nil}, expected: wsstarter.ErrRouterNil},
		{name: "empty path", routers: []*wsstarter.Router{{Handler: func(wsstarter.Message, *wsstarter.Conn) {}}}, expected: wsstarter.ErrRouterPathMissing},
		{name: "missing handler", routers: []*wsstarter.Router{{Path: "/ws"}}, expected: wsstarter.ErrRouterHandlerMissing},
		{
			name: "duplicate path",
			routers: []*wsstarter.Router{
				{Path: "/ws", Handler: func(wsstarter.Message, *wsstarter.Conn) {}},
				{Path: "/ws", Handler: func(wsstarter.Message, *wsstarter.Conn) {}},
			},
			expected: wsstarter.ErrRouterPathDuplicate,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			starter := &wsstarter.WebsocketStarter{Config: wsstarter.WebsocketConfig{ListenAddress: "127.0.0.1:0", Routers: test.routers}}
			if _, err := starter.Start(); !errors.Is(err, test.expected) {
				t.Fatalf("expected %v, got: %v", test.expected, err)
			}
		})
	}
}

func TestServerValidatesKeepAliveConfiguration(t *testing.T) {
	starter := &wsstarter.WebsocketStarter{Config: wsstarter.WebsocketConfig{
		ListenAddress: "127.0.0.1:0",
		Routers: []*wsstarter.Router{
			{Path: "/ws", Handler: func(wsstarter.Message, *wsstarter.Conn) {}},
		},
		DefaultKeepAliveConfig: &wsstarter.DefaultKeepAliveConfig{
			PingTimeout:    time.Second,
			MaxConnectTime: -time.Second,
		},
	}}
	if _, err := starter.Start(); !errors.Is(err, wsstarter.ErrKeepAliveMaxConnectTimeInvalid) {
		t.Fatalf("expected invalid max connection time error, got: %v", err)
	}
}
