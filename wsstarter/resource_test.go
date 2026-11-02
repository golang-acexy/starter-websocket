package wsstarter

import (
	"context"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestCloseReentrantCallback(t *testing.T) {
	var client *WSClient
	called := make(chan struct{})
	client = NewWSClient(context.Background(), WSClientConfig{OnClosed: func(error) { _ = client.Close(); close(called) }})
	go func() { _ = client.Close() }()
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("OnClosed reentrant Close deadlocked")
	}
}

func TestCloseReleasesPendingSends(t *testing.T) {
	client := NewWSClient(context.Background(), WSClientConfig{SendChanBufferLen: 16})
	client.setState(StateConnected)
	for i := 0; i < 16; i++ {
		if err := client.Send(websocket.MessageBinary, make([]byte, 65536)); err != nil {
			t.Fatal(err)
		}
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if len(client.sendChan) != 0 {
		t.Fatalf("closed client retains %d queued messages (1 MiB payload)", len(client.sendChan))
	}
}

func TestReceiveClosesWhenLateWorkerExits(t *testing.T) {
	client := NewWSClient(context.Background(), WSClientConfig{})
	if !client.addWorker() {
		t.Fatal("worker registration failed")
	}
	done := make(chan struct{})
	go func() { _ = client.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(7 * time.Second):
		client.workerWg.Done()
		t.Fatal("Close did not respect worker wait")
	}
	client.workerWg.Done()
	select {
	case _, ok := <-client.receiveChan:
		if ok {
			t.Fatal("unexpected queued message")
		}
	case <-time.After(time.Second):
		t.Fatal("receive channel stayed open after late worker exit")
	}
}
