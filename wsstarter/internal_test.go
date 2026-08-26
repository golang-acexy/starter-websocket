package wsstarter

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestCloneWebsocketConfigIsolatesMutableOptions(t *testing.T) {
	config := WebsocketConfig{
		AcceptOptions:          &websocket.AcceptOptions{},
		DefaultKeepAliveConfig: &DefaultKeepAliveConfig{PingTimeout: time.Second},
		Routers: []*Router{
			{Path: "/ws", Handler: func(Message, *Conn) {}},
		},
	}
	cloned := cloneWebsocketConfig(config)
	config.AcceptOptions.InsecureSkipVerify = true
	config.DefaultKeepAliveConfig.PingTimeout = time.Minute
	config.Routers[0].Path = "/changed"

	if cloned.AcceptOptions.InsecureSkipVerify {
		t.Fatal("accept options should be cloned")
	}
	if cloned.DefaultKeepAliveConfig.PingTimeout != time.Second {
		t.Fatal("keepalive config should be cloned")
	}
	if cloned.Routers[0].Path != "/ws" {
		t.Fatal("routers should be cloned")
	}
}

func TestUniqueConnectionRegistrationReplacesAtomically(t *testing.T) {
	handler := &handlerWrapper{
		uniqueConnId: true,
		allConn:      make(map[string]map[string]*Conn),
	}
	first := &Conn{ConnId: "same", internalConnId: "first"}
	second := &Conn{ConnId: "same", internalConnId: "second"}
	if replaced := handler.saveConn("same", "first", first); len(replaced) != 0 {
		t.Fatalf("unexpected initial replacement: %+v", replaced)
	}
	replaced := handler.saveConn("same", "second", second)
	if len(replaced) != 1 || replaced[0] != first {
		t.Fatalf("unexpected replaced connections: %+v", replaced)
	}
	actual, ok := handler.getConn("same", "second")
	if !ok || actual != second {
		t.Fatalf("unexpected active connection: %+v", actual)
	}
}

func TestClientStateTransitionsAreAtomic(t *testing.T) {
	client := NewWSClient(context.Background(), WSClientConfig{})
	defer client.Close()
	if !client.transitionState(StateWaitToConnect, StateConnecting) {
		t.Fatal("initial transition should succeed")
	}
	if client.transitionState(StateWaitToConnect, StateConnecting) {
		t.Fatal("stale transition should fail")
	}
	if client.GetState() != StateConnecting {
		t.Fatalf("unexpected state: %v", client.GetState())
	}
}

func TestInvalidServeMuxPatternReturnsError(t *testing.T) {
	starter := &WebsocketStarter{Config: WebsocketConfig{
		ListenAddress: "127.0.0.1:0",
		Routers: []*Router{
			{Path: "GET /{broken", Handler: func(Message, *Conn) {}},
		},
	}}
	if _, err := starter.Start(); !errors.Is(err, ErrRouterPathInvalid) {
		t.Fatalf("expected invalid route path error, got: %v", err)
	}
}
