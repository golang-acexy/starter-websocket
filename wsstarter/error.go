package wsstarter

import "errors"

var (
	// ErrWebsocketServerAlreadyStarted reports another process-wide server lifecycle already active.
	ErrWebsocketServerAlreadyStarted = errors.New("websocket server already started")
	// ErrWebsocketServerNotStarted reports no running server available to stop.
	ErrWebsocketServerNotStarted = errors.New("websocket server not started")
	// ErrMissRouters reports an empty server route list.
	ErrMissRouters = errors.New("no routers configured")
	// ErrRouterNil reports a nil route entry.
	ErrRouterNil = errors.New("router is nil")
	// ErrRouterPathMissing reports an empty route pattern.
	ErrRouterPathMissing = errors.New("router path is empty")
	// ErrRouterPathDuplicate reports a duplicate route pattern.
	ErrRouterPathDuplicate = errors.New("duplicate router path")
	// ErrRouterPathInvalid reports ServeMux pattern registration failure.
	ErrRouterPathInvalid = errors.New("invalid router path")
	// ErrKeepAlivePingTimeoutRequired reports a nonpositive enabled keepalive ping timeout.
	ErrKeepAlivePingTimeoutRequired = errors.New("default keep alive config ping timeout must be greater than 0")
	// ErrKeepAliveMaxConnectTimeInvalid reports a negative connection lifetime limit.
	ErrKeepAliveMaxConnectTimeInvalid = errors.New("default keep alive max connect time must not be negative")
	// ErrRouterHandlerMissing reports a route with no message handler.
	ErrRouterHandlerMissing = errors.New("router handler is missing")
	// ErrClientNotWaitToConnect reports Connect outside the initial waiting state.
	ErrClientNotWaitToConnect = errors.New("client is not in wait to connect state")
	// ErrClientNotConnected reports a send or ping outside connected state.
	ErrClientNotConnected = errors.New("client is not connected")
	// ErrClientClosing reports cancellation or closure interrupting client work.
	ErrClientClosing = errors.New("client is closing")
	// ErrSendChannelFull reports a full nonblocking send queue.
	ErrSendChannelFull = errors.New("send channel is full")
	// ErrConnectionNil reports a missing transport despite a connected-state observation.
	ErrConnectionNil = errors.New("connection is nil")
	// ErrReconnectAttemptsExhausted reports failure of every configured automatic retry.
	ErrReconnectAttemptsExhausted = errors.New("reconnection failed after all attempts")
	// ErrClientURLMissing reports an empty client endpoint.
	ErrClientURLMissing = errors.New("client URL is empty")
	// ErrReconnectAttemptsInvalid reports a negative retry count; zero selects the default.
	ErrReconnectAttemptsInvalid = errors.New("max reconnect attempts must be greater than 0")
	// ErrReconnectIntervalInvalid reports a negative retry interval; zero selects the default.
	ErrReconnectIntervalInvalid = errors.New("reconnect interval must be greater than 0")
)
