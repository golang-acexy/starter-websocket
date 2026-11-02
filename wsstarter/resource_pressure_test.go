package wsstarter

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"weak"

	"github.com/coder/websocket"
)

type delayedHijackWriter struct {
	http.ResponseWriter
	entered chan struct{}
	release chan struct{}
}

func (w *delayedHijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := w.ResponseWriter.(http.Hijacker).Hijack()
	close(w.entered)
	<-w.release
	return conn, rw, err
}

func TestShutdownRejectsLateUpgrade(t *testing.T) {
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	config := &WebsocketConfig{}
	h := &handlerWrapper{allConn: make(map[string]map[string]*Conn), handler: func(Message, *Conn) {}, config: config}
	previous := websocketRuntimeState.Swap(&websocketRuntime{config: config})
	defer websocketRuntimeState.Store(previous)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(finished)
		h.ServeHTTP(&delayedHijackWriter{w, entered, release}, r)
	}))
	defer server.Close()
	result := make(chan *websocket.Conn, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		conn, _, _ := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
		result <- conn
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("handshake did not reach hijack")
	}
	h.closeAllConnections()
	close(release)
	conn := <-result
	if conn != nil {
		defer conn.CloseNow()
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		if conn != nil {
			_ = conn.CloseNow()
		}
		t.Fatal("late upgrade survived registry shutdown")
	}
	h.mux.RLock()
	count := len(h.allConn)
	h.mux.RUnlock()
	if count != 0 {
		t.Fatalf("stopped registry contains %d identities", count)
	}
}

func TestStaleServerCleanupPreservesNewLifecycle(t *testing.T) {
	starter := &WebsocketStarter{Config: WebsocketConfig{ListenAddress: "127.0.0.1:0", Routers: []*Router{{Path: "/ws", Handler: func(Message, *Conn) {}}}}}
	if _, err := starter.Start(); err != nil {
		t.Fatal(err)
	}
	old := websocketRuntimeState.Load()
	if _, _, err := starter.Stop(time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := starter.Start(); err != nil {
		t.Fatal(err)
	}
	current := websocketRuntimeState.Load()
	defer func() { _, _, _ = starter.Stop(time.Second); _ = current.server.Close() }()
	clearWebsocketServer(old)
	serverLifecycleLock.Lock()
	state := serverState
	serverLifecycleLock.Unlock()
	if state != websocketRunning || websocketRuntimeState.Load() != current {
		t.Fatal("old cleanup changed new server lifecycle")
	}
}

func TestWebsocketResourcePressure(t *testing.T) {
	if testing.Short() {
		t.Skip("local TCP resource pressure")
	}
	runtime.GC()
	baseline := runtime.NumGoroutine()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	var clients []weak.Pointer[WSClient]
	var connections []weak.Pointer[Conn]
	var mu sync.Mutex
	for round := 0; round < 6; round++ {
		starter := &WebsocketStarter{Config: WebsocketConfig{ListenAddress: "127.0.0.1:0", DefaultKeepAliveConfig: &DefaultKeepAliveConfig{PingTimeout: time.Minute}, Routers: []*Router{{Path: "/ws", Handler: func(message Message, c *Conn) {
			if string(message.Data) == "first" {
				mu.Lock()
				connections = append(connections, weak.Make(c))
				mu.Unlock()
			}
			_ = c.SendMessage(message)
		}}}}}
		raw, err := starter.Start()
		if err != nil {
			t.Fatal(err)
		}
		endpoint := "ws://" + raw.(*http.Server).Addr + "/ws"
		var workers sync.WaitGroup
		for i := 0; i < 12; i++ {
			workers.Add(1)
			go func() {
				defer workers.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				client := NewWSClient(ctx, WSClientConfig{URL: endpoint, DisableReconnect: true, BlockReceive: true, BlockSender: true, ReceiveChanBufferLen: 8, SendChanBufferLen: 8})
				defer client.Close()
				mu.Lock()
				clients = append(clients, weak.Make(client))
				mu.Unlock()
				receive, err := client.Connect()
				if err != nil {
					t.Error(err)
					return
				}
				for j := 0; j < 64; j++ {
					data := "first"
					if j > 0 {
						data = strings.Repeat("x", 1024)
					}
					if err := client.SendText(data); err != nil {
						t.Error(err)
						return
					}
					select {
					case message, ok := <-receive:
						if !ok || string(message.Data) != data {
							t.Error("echo mismatch")
							return
						}
					case <-ctx.Done():
						t.Error("echo timed out")
						return
					}
				}
			}()
		}
		workers.Wait()
		if _, stopped, err := starter.Stop(3 * time.Second); err != nil || !stopped {
			t.Fatalf("stop: %v %v", stopped, err)
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	live := func() int {
		n := 0
		for _, p := range clients {
			if p.Value() != nil {
				n++
			}
		}
		for _, p := range connections {
			if p.Value() != nil {
				n++
			}
		}
		return n
	}
	for {
		runtime.GC()
		if live() == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d client/server wrappers retained", live())
		}
		time.Sleep(20 * time.Millisecond)
	}
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	t.Logf("6 server lifetimes, %d clients, %d server connections, 4608 echoes; live wrappers=0; goroutines %d->%d; heap %d->%d", len(clients), len(connections), baseline, runtime.NumGoroutine(), before.HeapAlloc, after.HeapAlloc)
}

func BenchmarkClientSendQueue(b *testing.B) {
	for _, size := range []int{64, 65536} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			c := NewWSClient(context.Background(), WSClientConfig{SendChanBufferLen: 1})
			defer c.Close()
			c.setState(StateConnected)
			payload := make([]byte, size)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if err := c.SendBinary(payload); err != nil {
					b.Fatal(err)
				}
				<-c.sendChan
			}
		})
	}
}

func TestFailedProxyDialsReleaseIdleConnections(t *testing.T) {
	var mu sync.Mutex
	closed := 0
	proxy := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "test rejection", http.StatusForbidden) }))
	proxy.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			mu.Lock()
			closed++
			mu.Unlock()
		}
	}
	proxy.Start()
	defer proxy.Close()
	for i := 0; i < 12; i++ {
		c := NewWSClient(context.Background(), WSClientConfig{URL: "ws://unused.invalid/ws", HttpProxyURL: proxy.URL})
		_, err := c.Connect()
		_ = c.Close()
		if err == nil {
			t.Fatal("proxy rejection unexpectedly succeeded")
		}
	}
	deadline := time.Now().Add(time.Second)
	for {
		mu.Lock()
		count := closed
		mu.Unlock()
		if count == 12 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d/12 failed-dial proxy connections closed", count)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestReconnectChurnAndClose(t *testing.T) {
	peers := make(chan *websocket.Conn, 32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		peers <- conn
		for {
			typ, data, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			if err = conn.Write(r.Context(), typ, data); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connected := make(chan struct{}, 32)
	c := NewWSClient(ctx, WSClientConfig{URL: "ws" + strings.TrimPrefix(server.URL, "http"), ForceReconnect: true, ReconnectInterval: time.Millisecond, OnConnected: func() { connected <- struct{}{} }})
	defer c.Close()
	messages, err := c.Connect()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 21; i++ {
		select {
		case <-connected:
		case <-ctx.Done():
			t.Fatal("reconnect timeout")
		}
		var peer *websocket.Conn
		select {
		case peer = <-peers:
		case <-ctx.Done():
			t.Fatal("server peer missing")
		}
		if err := c.SendText("echo"); err != nil {
			t.Fatal(err)
		}
		select {
		case m := <-messages:
			if m == nil || string(m.Data) != "echo" {
				t.Fatal("reconnect echo mismatch")
			}
		case <-ctx.Done():
			t.Fatal("reconnect echo timeout")
		}
		if i < 20 {
			_ = peer.CloseNow()
		}
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case _, ok := <-messages:
		if ok {
			t.Fatal("unexpected remaining message")
		}
	case <-ctx.Done():
		t.Fatal("receive channel did not close")
	}
}
