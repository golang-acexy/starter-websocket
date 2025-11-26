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

type WebsocketConfig struct {
	ListenAddress string // ip:port
	// websocket.AcceptOptions 原始参数设置 注意当设置DefaultKeepAliveConfig/CustomKeepAliveConfig后 OnPingReceived & OnPongReceived 设置将被忽略
	AcceptOptions  *websocket.AcceptOptions
	ConnIdentifier ConnIdentifier

	GlobalConnIdentifier ConnIdentifier // 全局连接标识符/鉴权操作 将覆盖未设置该行为的router
	Routers              []*Router      // WS路由设置

	DefaultKeepAliveConfig *DefaultKeepAliveConfig // 默认的KeepAlive配置 如果不设置则不起用该规则
	CustomKeepAliveConfig  *CustomKeepAliveConfig  // TODO: 自定义的KeepAlive配置 该配置高于默认规则 只生效一个
}

// DefaultKeepAliveConfig 默认的KeepAlive配置
// 默认连接保持采用被动模式
// 客户端需要在指定时间内发送ws的ping帧，服务端自动回复ws的pong帧
// 如果超过指定时间没有收到ping帧，则主动断开连接
type DefaultKeepAliveConfig struct {
	PingTimeout    time.Duration // ping帧的超时时间
	MaxConnectTime time.Duration // 连接保持最大时长 不设置时则不启用该规则
}

// CustomKeepAliveConfig 自定义的KeepAlive配置
type CustomKeepAliveConfig struct{}

type WebsocketStarter struct {
	Config     WebsocketConfig
	LazyConfig func() WebsocketConfig

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
	config := w.getConfig()
	if len(config.Routers) == 0 {
		return nil, errors.New("miss routers")
	}

	// 检查配置
	if config.DefaultKeepAliveConfig != nil && config.CustomKeepAliveConfig == nil && config.DefaultKeepAliveConfig.PingTimeout == 0 {
		return nil, errors.New("default keep alive config ping timeout must be greater than 0")
	}

	listenAddr := config.ListenAddress
	serveMux := http.NewServeMux()
	var err error
	coll.SliceForeach(config.Routers, func(router *Router) bool {
		if router.Handler == nil {
			err = errors.New("path miss handler: " + router.Path)
			return false
		}
		serveMux.Handle(router.Path, &handlerWrapper{
			connIdentifier: func() ConnIdentifier {
				if config.GlobalConnIdentifier != nil && router.ConnIdentifier == nil {
					return config.GlobalConnIdentifier
				}
				return router.ConnIdentifier
			}(),
			handler:      router.Handler,
			uniqueConnId: router.UniqueConnId,
			allConn:      make(map[string]map[string]*Conn),
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
		Addr:    listenAddr,
		Handler: serveMux,
	}

	errChn := make(chan error)
	go func() {
		if err = server.ListenAndServe(); err != nil {
			errChn <- err
		}
	}()
	select {
	case <-time.After(time.Second):
		return server, nil
	case err = <-errChn:
		return server, err
	}
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
