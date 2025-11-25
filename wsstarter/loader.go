package wsstarter

import (
	"time"

	"github.com/golang-acexy/starter-parent/parent"
)

type WebsocketConfig struct {
}

type WebsocketStarter struct {
	Config           WebsocketConfig
	LazyConfig       func() WebsocketConfig
	config           *WebsocketConfig
	WebsocketSetting *parent.Setting
}

func (w *WebsocketStarter) Setting() *parent.Setting {
	if w.WebsocketSetting != nil {
		return w.WebsocketSetting
	}
	return parent.NewSetting(
		"Websocket-Starter",
		1,
		false,
		time.Second*30,
		func(instance any) {
		})
}

func (w *WebsocketStarter) Start() (any, error) {
	//TODO implement me
	panic("implement me")
}

func (w *WebsocketStarter) Stop(maxWaitTime time.Duration) (gracefully, stopped bool, err error) {
	//TODO implement me
	panic("implement me")
}
