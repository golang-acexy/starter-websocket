package test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/acexy/golang-toolkit/sys"
	"github.com/acexy/golang-toolkit/util/str"
	"github.com/golang-acexy/starter-parent/parent"
	"github.com/golang-acexy/starter-websocket/wsstarter"
)

func TestServer(t *testing.T) {
	loader := parent.NewStarterLoader([]parent.Starter{
		&wsstarter.WebsocketStarter{
			Config: wsstarter.WebsocketConfig{
				ListenAddress: ":8081",
				Routers: []*wsstarter.Router{
					{
						Path: "/ws",
						Handler: func(message wsstarter.Message, conn *wsstarter.Conn) {
							fmt.Println(conn.ConnId, message.ToString())
							if message.ToString() == "close" {
								conn.Close()
							}
							if message.ToString() == "stream" {
								streamMessage, _ := conn.SendStreamTextMessage(context.Background())
								streamMessage.Write([]byte("hello world"))
								time.Sleep(time.Second)
								streamMessage.Write([]byte("bye"))
								streamMessage.Close()
							}
						},
					},
				},
				GlobalIdentifier: func(request *wsstarter.Request) (string, error) {
					if !str.HasText(request.GetQuery("id")) {
						return "", errors.New("miss id")
					}
					return "", nil
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
