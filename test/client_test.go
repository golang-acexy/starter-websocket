package test

import (
	"context"
	"testing"

	"github.com/acexy/golang-toolkit/logger"
	"github.com/acexy/golang-toolkit/sys"
	"github.com/golang-acexy/starter-websocket/wsstarter"
)

func TestClient(t *testing.T) {
	logger.EnableConsole(logger.TraceLevel, false)
	ctx, cancel := context.WithCancel(context.Background())
	client := wsstarter.NewWSClient(ctx, wsstarter.WSClientConfig{
		URL:          "wss://",
		HttpProxyURL: "http://localhost:7890",
		OnConnected: func() {
			logger.Logrus().Infoln("ws connected")
		},
		OnDisconnected: func(err error) {
			logger.Logrus().Infoln("ws disconnected", err)
		},
		OnClosed: func(err error) {
			cancel()
		},
	})
	//client.SetHeartbeat(time.Second*30, "ping", "pong")
	dataChn, err := client.Connect()
	if err != nil {
		t.Error(err)
		cancel()
		return
	}
	go func() {
		for {
			select {
			case d := <-dataChn:
				logger.Logrus().Debugln(d.ToString())
			case <-ctx.Done():
				return
			}
		}
	}()
	sys.ShutdownHolding()
	cancel()
	client.Close()
}
