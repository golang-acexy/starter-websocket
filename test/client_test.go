package test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/acexy/golang-toolkit/logger"
	"github.com/acexy/golang-toolkit/sys"
	"github.com/golang-acexy/starter-websocket/wsstarter"
)

func TestClient(t *testing.T) {
	logger.EnableConsole(logger.TraceLevel, false)
	ctx, cancel := context.WithCancel(context.Background())
	client := wsstarter.NewWSClient(ctx, wsstarter.WSClientConfig{
		URL:          "wss://fstream.binance.com/ws/btcusdt@markPrice@1s",
		HttpProxyURL: "http://localhost:7890",
		OnConnected: func() {
			logger.Logrus().Infoln("ws connected")
		},
		OnDisconnected: func(err error) {
			logger.Logrus().Infoln("ws disconnected", err)
		},
		OnClosed: func(err error) {
			logger.Logrus().Infoln("ws closed")
		},
	})
	//client.SetHeartbeat(time.Second*30, "ping", "pong")
	dataChn, err := client.Connect()
	if err != nil {
		t.Error(err)
		cancel()
		return
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for v := range dataChn {
			fmt.Println(v)
		}
	}()
	sys.ShutdownHolding()
	cancel()

	//client.Close()
	wg.Wait()
}
