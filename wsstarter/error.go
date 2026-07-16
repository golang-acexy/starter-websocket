package wsstarter

import "errors"

var (
	ErrWebsocketServerAlreadyStarted = errors.New("websocket server already started")
	ErrWebsocketServerNotStarted     = errors.New("websocket server not started")
	ErrMissRouters                   = errors.New("miss routers")
	ErrKeepAlivePingTimeoutRequired  = errors.New("default keep alive config ping timeout must be greater than 0")
	ErrRouterHandlerMissing          = errors.New("path miss handler")
	ErrClientNotWaitToConnect        = errors.New("client is not in wait to connect state")
	ErrClientNotConnected            = errors.New("client is not connected")
	ErrClientClosing                 = errors.New("client is closing")
	ErrSendChannelFull               = errors.New("send channel is full")
	ErrConnectionNil                 = errors.New("connection is nil")
	ErrReconnectAttemptsExhausted    = errors.New("reconnection failed after all attempts")
)
