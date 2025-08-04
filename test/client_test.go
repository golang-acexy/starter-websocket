package test

import (
	"context"
	"github.com/acexy/golang-toolkit/logger"
	"github.com/acexy/golang-toolkit/sys"
	"github.com/golang-acexy/starter-websocket/wsstarter"
	"testing"
	"time"
)

func TestClient(t *testing.T) {
	logger.EnableConsole(logger.TraceLevel, false)
	ctx := context.Background()
	client := wsstarter.NewWSClient(ctx, wsstarter.WSClientConfig{
		URL: "wss://xxx",
		OnConnect: func() {
			t.Log("connect")
		},
		OnDisconnect: func(err error) {
			t.Log("disconnect", err)
		},
	})
	client.SetHeartbeat(time.Second*30, "ping", "pong")
	chn, err := client.Connect()
	if err != nil {
		t.Error(err)
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
	_ = client.Close()
}
