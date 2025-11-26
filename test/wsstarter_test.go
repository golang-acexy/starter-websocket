package test

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/acexy/golang-toolkit/logger"
	"github.com/acexy/golang-toolkit/sys"
	"github.com/acexy/golang-toolkit/util/str"
	"github.com/golang-acexy/starter-parent/parent"
	"github.com/golang-acexy/starter-websocket/wsstarter"
)

func TestServer(t *testing.T) {
	logger.EnableConsole(logger.TraceLevel)
	loader := parent.NewStarterLoader([]parent.Starter{
		&wsstarter.WebsocketStarter{
			Config: wsstarter.WebsocketConfig{
				ListenAddress: ":8081",
				Routers: []*wsstarter.Router{
					{
						Path:         "/ws",
						UniqueConnId: true,
						Handler: func(message wsstarter.Message, conn *wsstarter.Conn) {
							fmt.Println(conn.ConnId, message.ToString())
							if message.ToString() == "close" {
								conn.Close()
							}
							if message.ToString() == "stream" {
								writer, _ := conn.GetTextMessageWriter()
								writer.Write([]byte("hello world"))
								writer.Write([]byte("bye"))
								writer.Close()
							}
						},
					},
					{
						Path: "/ws1",
						ConnIdentifier: func(request *wsstarter.Request) (string, error) {
							return "123456", nil
						},
						Handler: func(message wsstarter.Message, conn *wsstarter.Conn) {
							fmt.Println(conn.ConnId, message.ToString())
						},
					},
				},
				GlobalConnIdentifier: func(request *wsstarter.Request) (string, error) {
					if !str.HasText(request.GetQuery("id")) {
						return "", errors.New("miss id")
					}
					return request.GetQuery("id"), nil
				},
				DefaultKeepAliveConfig: &wsstarter.DefaultKeepAliveConfig{
					PingTimeout: 5 * time.Second,
				},
			},
		},
	})
	err := loader.Start()
	if err != nil {
		t.Fatal(err)
	}
	sys.ShutdownCallback(func() {
		loader.StopBySetting()
	})
}
