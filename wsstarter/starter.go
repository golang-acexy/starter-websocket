package wsstarter

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/acexy/golang-toolkit/logger"
	"github.com/acexy/golang-toolkit/util/coll"
	"github.com/coder/websocket"
	"github.com/golang-acexy/starter-parent/parent"
)

var websocketRuntimeState atomic.Pointer[websocketRuntime]
var serverLifecycleLock sync.Mutex
var serverState websocketLifecycleState

type websocketRuntime struct {
	config   *WebsocketConfig
	server   *http.Server
	handlers []*handlerWrapper
	done     <-chan struct{}
}

type websocketLifecycleState uint8

const (
	websocketStopped websocketLifecycleState = iota
	websocketStarting
	websocketRunning
	websocketStopping
)

// WebsocketConfig defines one server's routes and handshake policies.
// Configure it before first resolution; later changes do not refresh the starter's cache.
type WebsocketConfig struct {
	// ListenAddress is the TCP bind address; empty uses :8081 and port zero selects a free port.
	ListenAddress string
	// AcceptOptions configures handshakes; nil uses library defaults. Keepalive overrides OnPingReceived.
	AcceptOptions *websocket.AcceptOptions
	// ConnIdentifier is the fallback identifier after router and global identifiers.
	ConnIdentifier ConnIdentifier

	// GlobalConnIdentifier overrides the fallback for routes without their own identifier.
	GlobalConnIdentifier ConnIdentifier
	// Routers must contain non-nil routes with unique non-empty patterns and non-nil handlers.
	Routers []*Router

	// DefaultKeepAliveConfig enables passive ping checks; nil disables both timeout and lifetime checks.
	DefaultKeepAliveConfig *DefaultKeepAliveConfig
}

// DefaultKeepAliveConfig closes connections missing client ping frames or exceeding their lifetime.
// Application messages do not refresh the ping deadline; clients must schedule pings.
type DefaultKeepAliveConfig struct {
	// PingTimeout must be positive; before the first ping it is measured from connection creation.
	PingTimeout time.Duration
	// MaxConnectTime limits connection lifetime; zero disables the limit and negative values are invalid.
	MaxConnectTime time.Duration
}

// WebsocketStarter manages the process-wide WebSocket HTTP server.
// Use one server lifecycle owner, configure fields before use, and do not copy after use.
// Restart is supported and reuses the first resolved configuration.
type WebsocketStarter struct {
	// Config supplies static settings when LazyConfig is nil.
	Config WebsocketConfig
	// LazyConfig takes precedence over Config and is evaluated once; it must not recursively resolve this
	// starter.
	LazyConfig func() WebsocketConfig

	config     *WebsocketConfig
	configLock sync.Mutex
	// WebsocketSetting overrides parent lifecycle defaults; nil enables restart and a 30-second synchronous
	// stop.
	WebsocketSetting *parent.Setting
}

// getConfig resolves and caches a cloned configuration under the instance lock.
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

// cloneWebsocketConfig isolates routes and mutable option slices without cloning callback-owned state.
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

// Setting returns custom parent settings or restart-enabled defaults.
// Configure WebsocketSetting before use; defaults use name Websocket-Starter and priority one.
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

// Start validates cached configuration, binds TCP, and returns the raw *http.Server.
// The server is process-wide; another active lifecycle returns ErrWebsocketServerAlreadyStarted.
// Binding and route failures are returned before runtime publication. Later Serve failures
// are logged and are not returned through this method. Restart keeps the cached configuration.
func (w *WebsocketStarter) Start() (any, error) {
	config := w.getConfig()
	if err := validateWebsocketConfig(config); err != nil {
		return nil, err
	}
	serverLifecycleLock.Lock()
	if serverState != websocketStopped {
		current := websocketRuntimeState.Load()
		serverLifecycleLock.Unlock()
		if current != nil {
			return current.server, ErrWebsocketServerAlreadyStarted
		}
		return nil, ErrWebsocketServerAlreadyStarted
	}
	serverState = websocketStarting
	serverLifecycleLock.Unlock()
	started := false
	defer func() {
		if !started {
			serverLifecycleLock.Lock()
			serverState = websocketStopped
			serverLifecycleLock.Unlock()
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
	runtime := &websocketRuntime{config: config, server: wsServer, handlers: handlers, done: done}
	serverLifecycleLock.Lock()
	websocketRuntimeState.Store(runtime)
	serverState = websocketRunning
	serverLifecycleLock.Unlock()
	started = true

	go func() {
		defer close(done)
		if serveErr := wsServer.Serve(listener); serveErr != nil && serveErr != http.ErrServerClosed {
			logger.Logrus().WithError(serveErr).Errorln("websocket server stopped unexpectedly")
		}
		coll.SliceForEachAll(handlers, func(handler *handlerWrapper) {
			handler.closeAllConnections()
		})
		clearWebsocketServer(runtime)
	}()
	return wsServer, nil
}

// Stop withdraws the process-wide server, closes upgraded connections, and stops HTTP serving.
// maxWaitTime bounds HTTP shutdown and the subsequent Serve wait, not preceding connection cleanup.
// Zero or negative durations expire immediately; shutdown failure forces HTTP closure.
// An inactive server returns stopped=true with ErrWebsocketServerNotStarted.
// Inspect both flags and err: timeout can return stopped=false after initiating closure.
func (w *WebsocketStarter) Stop(maxWaitTime time.Duration) (gracefully, stopped bool, err error) {
	serverLifecycleLock.Lock()
	runtime := websocketRuntimeState.Load()
	if serverState != websocketRunning || runtime == nil {
		serverLifecycleLock.Unlock()
		return false, true, ErrWebsocketServerNotStarted
	}
	websocketRuntimeState.Store(nil)
	serverState = websocketStopping
	serverLifecycleLock.Unlock()

	coll.SliceForEachAll(runtime.handlers, func(handler *handlerWrapper) {
		handler.closeAllConnections()
	})
	ctx, cancel := context.WithTimeout(context.Background(), maxWaitTime)
	defer cancel()
	if err = runtime.server.Shutdown(ctx); err != nil {
		gracefully = false
		_ = runtime.server.Close()
	} else {
		gracefully = true
	}
	select {
	case <-runtime.done:
		stopped = true
	case <-ctx.Done():
		stopped = false
		if err == nil {
			err = ctx.Err()
		}
	}
	clearWebsocketServer(runtime)
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

// RawWebsocketServer returns the current HTTP server, or nil when unavailable or stopping.
// Lookup is synchronized but the returned pointer is borrowed and does not guarantee
// continued availability. Leave server lifecycle control to WebsocketStarter.
func RawWebsocketServer() *http.Server {
	runtime := websocketRuntimeState.Load()
	if runtime == nil {
		return nil
	}
	return runtime.server
}

func currentWebsocketServer() (*http.Server, []*handlerWrapper, <-chan struct{}) {
	runtime := websocketRuntimeState.Load()
	if runtime == nil {
		return nil, nil, nil
	}
	return runtime.server, coll.SliceCollect(runtime.handlers, func(handler *handlerWrapper) *handlerWrapper {
		return handler
	}), runtime.done
}

func currentWebsocketConfig() *WebsocketConfig {
	runtime := websocketRuntimeState.Load()
	if runtime == nil {
		return nil
	}
	return runtime.config
}

// clearWebsocketServer withdraws matching runtime state and resets lifecycle availability.
func clearWebsocketServer(runtime *websocketRuntime) {
	websocketRuntimeState.CompareAndSwap(runtime, nil)
	serverLifecycleLock.Lock()
	if serverState == websocketRunning || serverState == websocketStopping {
		serverState = websocketStopped
	}
	serverLifecycleLock.Unlock()
}
