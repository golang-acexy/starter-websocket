# starter-websocket

`starter-websocket` provides a parent-managed WebSocket starter and a reusable client based on `github.com/coder/websocket`.
It is designed for the golang-acexy starter ecosystem, where each starter owns one focused capability and is started by `starter-parent`.

## Ecosystem Role

This module covers long-lived bidirectional connections that do not fit the request/response model of `starter-gin` or `starter-grpc`. It includes both a parent-managed server and an independently reusable client.

## Requirements

- Go `1.26.7`
- `github.com/golang-acexy/starter-parent`

## Installation

```bash
go get github.com/golang-acexy/starter-websocket
```

## Server Mode

Server mode is started through `WebsocketStarter` and should be registered in the parent loader. The server is managed as a singleton: only one WebSocket HTTP server is expected in one process.

Supported capabilities:

- Multi-router registration with independent `Path`, `Handler`, and optional `ConnIdentifier`.
- Global connection identifier through `GlobalConnIdentifier`, used when a router does not define its own identifier.
- Unique connection IDs per router with `UniqueConnId`; a repeated ID replaces the old connection.
- Request wrapper helpers for reading query and header values.
- Text and binary message sending through `Conn.SendMessage`, `SendMessageCtx`, and streaming writers.
- Passive keepalive through client ping frames with `DefaultKeepAliveConfig.PingTimeout`.
- Optional maximum connection lifetime through `DefaultKeepAliveConfig.MaxConnectTime`.
- Raw `websocket.AcceptOptions` passthrough. When default keepalive is enabled, ping handling is owned by the starter.
- Raw server access through `RawWebsocketServer()`.

Configure non-nil routes with unique valid patterns, handlers, and positive keepalive ping timeouts before startup. Startup returns listener errors directly, and shutdown closes upgraded WebSocket connections as well as the HTTP server.

```go
starter := &wsstarter.WebsocketStarter{
    Config: wsstarter.WebsocketConfig{
        ListenAddress: ":8081",
        GlobalConnIdentifier: func(request *wsstarter.Request) (string, error) {
            return request.GetQuery("clientId"), nil
        },
        Routers: []*wsstarter.Router{
            {
                Path:         "/ws",
                UniqueConnId: true,
                Handler: func(message wsstarter.Message, conn *wsstarter.Conn) {
                    _ = conn.SendMessage(wsstarter.Message{
                        Type: message.Type,
                        Data: message.Data,
                    })
                },
            },
        },
        DefaultKeepAliveConfig: &wsstarter.DefaultKeepAliveConfig{
            PingTimeout: 30 * time.Second,
        },
    },
}

loader := parent.InitStarterLoader([]parent.Starter{starter})
err := loader.Start()
```

## Client Mode

`WSClient` provides connection lifecycle management, send and receive queues, optional proxy configuration, reconnect control, and lifecycle callbacks.

```go
ctx, cancel := context.WithCancel(context.Background())
defer cancel()

client := wsstarter.NewWSClient(ctx, wsstarter.WSClientConfig{
    URL:                  "ws://localhost:8081/ws?clientId=demo",
    MaxReconnectAttempts: 3,
    ReconnectInterval:    2 * time.Second,
    OnConnected:          func() {},
    OnDisconnected:       func(err error) {},
    OnError:              func(err error) {},
    OnClosed:             func(err error) {},
})

messages, err := client.Connect()
if err != nil {
    return err
}

_ = client.SendText("hello")
for message := range messages {
    _ = message.ToString()
}
```

Client configuration highlights:

- `HttpProxyURL` or `HttpProxyURLFn` for proxy selection.
- `DisableReconnect`, `ForceReconnect`, `MaxReconnectAttempts`, and `ReconnectInterval` for reconnect behavior.
- `BlockReceive` and `BlockSender` for blocking channel behavior.
- `ReceiveChanBufferLen`, `SendChanBufferLen`, and `ReadMaxBytesLimit` for queue and read limits.
- `SendText`, `SendBinary`, `Ping`, `PingContext`, `Close`, and `GetState` for common operations.

An initial connection failure does not automatically reconnect, and the same client cannot call `Connect` again; close it and construct a new client. After an established connection fails, `DisableReconnect` takes priority over `ForceReconnect`. Without forcing, normal server closure does not reconnect.

A successful send means queue acceptance, not delivery. Binary buffers are not copied; keep queued data unchanged. Full receive queues drop new messages unless `BlockReceive` is enabled. Blocking consumption can delay ping/pong processing. No periodic client heartbeat is scheduled; call `Ping` regularly when the server requires it.

Close clients explicitly and keep callbacks short. Do not recursively call `Close` from `OnClosed`, or synchronously close from worker callbacks. The receive channel may remain open if workers exceed the closure wait; consumers should also observe context cancellation or a closure callback. A disconnected client without retries remains open until explicitly closed.

## Error Handling

Common starter and client errors are defined in `wsstarter/error.go`, including duplicated server startup, nil or duplicate routers, invalid keepalive configuration, missing client URLs, invalid reconnect settings, invalid client state, closed clients, full send queues, and reconnect exhaustion.

## Development

Use the parent loader for server startup and shutdown in integration code. Keep each router focused on one WebSocket endpoint, and place connection authentication or ID extraction in `ConnIdentifier` rather than inside message handlers.

`WebsocketStarter` is the only standard starter that enables parent-managed restart. After `StopStarter` completes successfully, calling `StartStarter("Websocket-Starter")` starts it again with the configuration cached at first resolution. Later changes to `Config` or `LazyConfig` do not refresh that configuration. Other standard starters reject this lifecycle transition with `parent.ErrStarterRestartDisabled`.

Tests use local random ports and in-process WebSocket servers. They do not require external services, local proxies, fixed ports, or indefinite shutdown holding.
