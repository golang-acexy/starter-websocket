package wsstarter

import "errors"

var (
	ErrWebsocketServerAlreadyStarted = errors.New("websocket server already started")
	ErrWebsocketServerNotStarted     = errors.New("websocket server not started")
	ErrMissRouters                   = errors.New("no routers configured")
	ErrRouterNil                     = errors.New("router is nil")
	ErrRouterPathMissing             = errors.New("router path is empty")
	ErrRouterPathDuplicate           = errors.New("duplicate router path")
	ErrRouterPathInvalid             = errors.New("invalid router path")
	ErrKeepAlivePingTimeoutRequired  = errors.New("default keep alive config ping timeout must be greater than 0")
	ErrKeepAliveMaxConnectTimeInvalid = errors.New("default keep alive max connect time must not be negative")
	ErrRouterHandlerMissing          = errors.New("router handler is missing")
	ErrClientNotWaitToConnect        = errors.New("client is not in wait to connect state")
	ErrClientNotConnected            = errors.New("client is not connected")
	ErrClientClosing                 = errors.New("client is closing")
	ErrSendChannelFull               = errors.New("send channel is full")
	ErrConnectionNil                 = errors.New("connection is nil")
	ErrReconnectAttemptsExhausted    = errors.New("reconnection failed after all attempts")
	ErrClientURLMissing              = errors.New("client URL is empty")
	ErrReconnectAttemptsInvalid      = errors.New("max reconnect attempts must be greater than 0")
	ErrReconnectIntervalInvalid      = errors.New("reconnect interval must be greater than 0")
)
