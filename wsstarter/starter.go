package wsstarter

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/acexy/golang-toolkit/logger"
	"github.com/acexy/golang-toolkit/util/coll"
	"github.com/coder/websocket"
	"github.com/golang-acexy/starter-parent/parent"
)

var webSocketConfig *WebsocketConfig
var server *http.Server
var serverHandlers []*handlerWrapper
var serverDone <-chan struct{}
var serverStarting bool
var serverLock sync.RWMutex

type WebsocketConfig struct {
	ListenAddress string // ip:port
	// websocket.AcceptOptions 原始参数设置。设置DefaultKeepAliveConfig后OnPingReceived将由Starter接管。
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
	configLock       sync.Mutex
	WebsocketSetting *parent.Setting
}

func (w *WebsocketStarter) getConfig() *WebsocketConfig {
	w.configLock.Lock()
	defer w.configLock.Unlock()
	if w.config != nil {
		return w.config
	}
	var config WebsocketConfig
	if w.LazyConfig != nil {
		config = w.LazyConfig()
	} else {
		config = w.Config
	}
	w.config = cloneWebsocketConfig(config)
	return w.config
}

func cloneWebsocketConfig(config WebsocketConfig) *WebsocketConfig {
	cloned := config
	if config.AcceptOptions != nil {
		acceptOptions := *config.AcceptOptions
		acceptOptions.Subprotocols = append([]string(nil), config.AcceptOptions.Subprotocols...)
		acceptOptions.OriginPatterns = append([]string(nil), config.AcceptOptions.OriginPatterns...)
		cloned.AcceptOptions = &acceptOptions
	}
	if config.DefaultKeepAliveConfig != nil {
		keepAliveConfig := *config.DefaultKeepAliveConfig
		cloned.DefaultKeepAliveConfig = &keepAliveConfig
	}
	cloned.Routers = coll.SliceCollect(config.Routers, func(router *Router) *Router {
		if router == nil {
			return nil
		}
		clonedRouter := *router
		return &clonedRouter
	})
	return &cloned
}

func (w *WebsocketStarter) Setting() *parent.Setting {
	if w.WebsocketSetting != nil {
		return w.WebsocketSetting
	}
	return parent.NewSetting(
		"Websocket-Starter",
		true,
		1,
		false,
		time.Second*30,
		func(instance any) {
		})
}

func (w *WebsocketStarter) Start() (any, error) {
	config := w.getConfig()
	if err := validateWebsocketConfig(config); err != nil {
		return nil, err
	}
	serverLock.Lock()
	if server != nil || serverStarting {
		current := server
		serverLock.Unlock()
		return current, ErrWebsocketServerAlreadyStarted
	}
	serverStarting = true
	serverLock.Unlock()
	started := false
	defer func() {
		if !started {
			serverLock.Lock()
			serverStarting = false
			serverLock.Unlock()
		}
	}()

	listenAddr := config.ListenAddress
	if listenAddr == "" {
		listenAddr = ":8081"
	}
	serveMux := http.NewServeMux()
	handlers := coll.SliceCollect(config.Routers, func(router *Router) *handlerWrapper {
		return &handlerWrapper{
			connIdentifier: func() ConnIdentifier {
				if router.ConnIdentifier != nil {
					return router.ConnIdentifier
				}
				if config.GlobalConnIdentifier != nil {
					return config.GlobalConnIdentifier
				}
				return config.ConnIdentifier
			}(),
			handler:      router.Handler,
			uniqueConnId: router.UniqueConnId,
			allConn:      make(map[string]map[string]*Conn),
		}
	})
	if err := registerWebsocketRoutes(serveMux, config.Routers, handlers); err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, err
	}

	wsServer := &http.Server{
		Addr:    listener.Addr().String(),
		Handler: serveMux,
	}
	done := make(chan struct{})
	serverLock.Lock()
	server = wsServer
	serverHandlers = handlers
	serverDone = done
	serverStarting = false
	webSocketConfig = config
	serverLock.Unlock()
	started = true

	go func() {
		defer close(done)
		if serveErr := wsServer.Serve(listener); serveErr != nil && serveErr != http.ErrServerClosed {
			logger.Logrus().WithError(serveErr).Errorln("websocket server stopped unexpectedly")
		}
		coll.SliceForEachAll(handlers, func(handler *handlerWrapper) {
			handler.closeAllConnections()
		})
		clearWebsocketServer(wsServer)
	}()
	return wsServer, nil
}

func (w *WebsocketStarter) Stop(maxWaitTime time.Duration) (gracefully, stopped bool, err error) {
	wsServer, handlers, done := currentWebsocketServer()
	if wsServer == nil {
		return false, true, ErrWebsocketServerNotStarted
	}
	coll.SliceForEachAll(handlers, func(handler *handlerWrapper) {
		handler.closeAllConnections()
	})
	ctx, cancel := context.WithTimeout(context.Background(), maxWaitTime)
	defer cancel()
	if err = wsServer.Shutdown(ctx); err != nil {
		gracefully = false
		_ = wsServer.Close()
	} else {
		gracefully = true
	}
	select {
	case <-done:
		stopped = true
	case <-ctx.Done():
		stopped = false
		if err == nil {
			err = ctx.Err()
		}
	}
	if stopped {
		clearWebsocketServer(wsServer)
	}
	return
}

func validateWebsocketConfig(config *WebsocketConfig) error {
	if len(config.Routers) == 0 {
		return ErrMissRouters
	}
	if config.DefaultKeepAliveConfig != nil && config.DefaultKeepAliveConfig.PingTimeout <= 0 {
		return ErrKeepAlivePingTimeoutRequired
	}
	if config.DefaultKeepAliveConfig != nil && config.DefaultKeepAliveConfig.MaxConnectTime < 0 {
		return ErrKeepAliveMaxConnectTimeInvalid
	}
	paths := make(map[string]struct{}, len(config.Routers))
	for _, router := range config.Routers {
		if router == nil {
			return ErrRouterNil
		}
		if router.Path == "" {
			return ErrRouterPathMissing
		}
		if router.Handler == nil {
			return fmt.Errorf("%w: %s", ErrRouterHandlerMissing, router.Path)
		}
		if _, exists := paths[router.Path]; exists {
			return fmt.Errorf("%w: %s", ErrRouterPathDuplicate, router.Path)
		}
		paths[router.Path] = struct{}{}
	}
	return nil
}

func registerWebsocketRoutes(serveMux *http.ServeMux, routers []*Router, handlers []*handlerWrapper) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%w: %v", ErrRouterPathInvalid, recovered)
		}
	}()
	for index, router := range routers {
		serveMux.Handle(router.Path, handlers[index])
	}
	return nil
}

// RawWebsocketServer 获取原始websocket http server实例。
func RawWebsocketServer() *http.Server {
	serverLock.RLock()
	defer serverLock.RUnlock()
	return server
}

func currentWebsocketServer() (*http.Server, []*handlerWrapper, <-chan struct{}) {
	serverLock.RLock()
	defer serverLock.RUnlock()
	return server, coll.SliceCollect(serverHandlers, func(handler *handlerWrapper) *handlerWrapper {
		return handler
	}), serverDone
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
		serverHandlers = nil
		serverDone = nil
		webSocketConfig = nil
	}
}
