package wsstarter

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/acexy/golang-toolkit/util/coll"
	"github.com/acexy/golang-toolkit/util/net"
	"github.com/coder/websocket"
	"github.com/golang-acexy/starter-parent/parent"
)

var webSocketConfig *WebsocketConfig
var server *http.Server
var done chan struct{}

type WebsocketConfig struct {
	ListenAddress    string                   // ip:port
	AcceptOptions    *websocket.AcceptOptions // websocket.AcceptOptions
	GlobalIdentifier ConnIdentifier           // 全局连接标识符/鉴权操作 将覆盖未设置该行为的router
	Routers          []*Router                // WS路由
}

type WebsocketStarter struct {
	Config           WebsocketConfig
	LazyConfig       func() WebsocketConfig
	config           *WebsocketConfig
	WebsocketSetting *parent.Setting
}

func (w *WebsocketStarter) getConfig() *WebsocketConfig {
	if w.config != nil {
		return w.config
	}
	var config WebsocketConfig
	if w.LazyConfig != nil {
		config = w.LazyConfig()
	} else {
		config = w.Config
	}
	w.config = &config
	webSocketConfig = &config
	return w.config
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
	done = make(chan struct{})
	config := w.getConfig()
	if len(config.Routers) == 0 {
		return nil, errors.New("miss routers")
	}
	listenAddr := config.ListenAddress
	muxSrv := http.NewServeMux()
	var err error
	coll.SliceForeach(config.Routers, func(router *Router) bool {
		if router.Handler == nil {
			err = errors.New("path miss handler: " + router.Path)
			return false
		}
		muxSrv.Handle(router.Path, &handlerWrapper{
			identifier: func() ConnIdentifier {
				if config.GlobalIdentifier != nil && router.Identifier == nil {
					return config.GlobalIdentifier
				}
				return router.Identifier
			}(),
			handler: router.Handler,
		})
		return true
	})
	if err != nil {
		return nil, err
	}
	if listenAddr == "" {
		listenAddr = ":8081"
	}
	server = &http.Server{
		Addr:    ":8080",
		Handler: muxSrv,
	}

	err = server.ListenAndServe()
	return muxSrv, err
}

func (w *WebsocketStarter) Stop(maxWaitTime time.Duration) (gracefully, stopped bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), maxWaitTime)
	defer cancel()
	if err = server.Shutdown(ctx); err != nil {
		gracefully = false
	} else {
		gracefully = true
	}
	stopped = !net.Telnet(w.getConfig().ListenAddress, time.Second)
	return
}
