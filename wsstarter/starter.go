package wsstarter

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/acexy/golang-toolkit/util/coll"
	"github.com/acexy/golang-toolkit/util/net"
	"github.com/coder/websocket"
	"github.com/golang-acexy/starter-parent/parent"
)

var webSocketConfig *WebsocketConfig
var server *http.Server
var serverLock sync.RWMutex

type WebsocketConfig struct {
	ListenAddress string // ip:port
	// websocket.AcceptOptions 原始参数设置 注意当设置DefaultKeepAliveConfig后 OnPingReceived & OnPongReceived 设置将被忽略
	AcceptOptions  *websocket.AcceptOptions
	ConnIdentifier ConnIdentifier

	GlobalConnIdentifier ConnIdentifier // 全局连接标识符/鉴权操作 将覆盖未设置该行为的router
	Routers              []*Router      // WS路由设置

	DefaultKeepAliveConfig *DefaultKeepAliveConfig // 默认的KeepAlive配置 如果不设置则不起用该规则
}

// DefaultKeepAliveConfig 默认的KeepAlive配置
// 默认连接保持采用被动模式
// 客户端需要在指定时间内发送ws的ping帧，服务端自动回复ws的pong帧
// 如果超过指定时间没有收到ping帧，则主动断开连接
type DefaultKeepAliveConfig struct {
	PingTimeout    time.Duration // ping帧的超时时间
	MaxConnectTime time.Duration // 连接保持最大时长 不设置时则不启用该规则
}

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
	serverLock.Lock()
	webSocketConfig = &config
	serverLock.Unlock()
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
		return nil, ErrMissRouters
	}

	// 检查配置
	if config.DefaultKeepAliveConfig != nil && config.DefaultKeepAliveConfig.PingTimeout == 0 {
		return nil, ErrKeepAlivePingTimeoutRequired
	}
	serverLock.Lock()
	if server != nil {
		current := server
		serverLock.Unlock()
		return current, ErrWebsocketServerAlreadyStarted
	}
	serverLock.Unlock()

	listenAddr := config.ListenAddress
	serveMux := http.NewServeMux()
	var err error
	coll.SliceForEach(config.Routers, func(router *Router) bool {
		if router.Handler == nil {
			err = fmt.Errorf("%w: %s", ErrRouterHandlerMissing, router.Path)
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

	wsServer := &http.Server{
		Addr:    listenAddr,
		Handler: serveMux,
	}
	serverLock.Lock()
	server = wsServer
	serverLock.Unlock()

	errChn := make(chan error, 1)
	go func() {
		if listenErr := wsServer.ListenAndServe(); listenErr != nil && listenErr != http.ErrServerClosed {
			errChn <- listenErr
		}
	}()
	select {
	case <-time.After(time.Second):
		return wsServer, nil
	case err = <-errChn:
		clearWebsocketServer(wsServer)
		return wsServer, err
	}
}

func (w *WebsocketStarter) Stop(maxWaitTime time.Duration) (gracefully, stopped bool, err error) {
	wsServer := RawWebsocketServer()
	if wsServer == nil {
		return false, true, ErrWebsocketServerNotStarted
	}
	ctx, cancel := context.WithTimeout(context.Background(), maxWaitTime)
	defer cancel()
	if err = wsServer.Shutdown(ctx); err != nil {
		gracefully = false
	} else {
		gracefully = true
	}
	stopped = !net.Telnet(w.getConfig().ListenAddress, time.Second)
	clearWebsocketServer(wsServer)
	return
}

// RawWebsocketServer 获取原始websocket http server实例。
func RawWebsocketServer() *http.Server {
	serverLock.RLock()
	defer serverLock.RUnlock()
	return server
}

func currentWebsocketConfig() *WebsocketConfig {
	serverLock.RLock()
	defer serverLock.RUnlock()
	return webSocketConfig
}

func clearWebsocketServer(wsServer *http.Server) {
	serverLock.Lock()
	defer serverLock.Unlock()
	if server == wsServer {
		server = nil
		webSocketConfig = nil
	}
}
