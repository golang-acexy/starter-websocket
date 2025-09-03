package test

import (
	"context"
	"testing"
	"time"

	"github.com/acexy/golang-toolkit/logger"
	"github.com/acexy/golang-toolkit/sys"
	"github.com/golang-acexy/starter-websocket/wsstarter"
)

func TestClient(t *testing.T) {
	logger.EnableConsole(logger.TraceLevel, false)
	ctx, cancel := context.WithCancel(context.Background())
	client := wsstarter.NewWSClient(ctx, wsstarter.WSClientConfig{
		URL: "https://",
		OnConnect: func() {
			t.Log("connect")
		},
		OnDisconnect: func(err error) {
			t.Log("disconnect", err)
		},
		OnClose: func(err error) {
			cancel()
		},
	})
	client.SetHeartbeat(time.Second*30, "ping", "pong")
	chn, err := client.Connect()
	if err != nil {
		t.Error(err)
		cancel()
		return
	}
	go func() {
		for {
			select {
			case _ = <-chn:
			case <-ctx.Done():
				return
			}
		}
	}()
	sys.ShutdownHolding()
	cancel()
	_ = client.Close()
}
