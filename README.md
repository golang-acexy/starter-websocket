# starter-websocket

`starter-websocket` provides a parent-managed WebSocket starter and a reusable client based on `github.com/coder/websocket`.
It is designed for the golang-acexy starter ecosystem, where each starter owns one focused capability and is started by `starter-parent`.

## Requirements

- Go `1.25.8`
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
_, err := loader.Start()
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
- `SendText`, `SendBinary`, `Ping`, `Close`, and `GetState` for common operations.

## Error Handling

Common starter and client errors are defined in `wsstarter/error.go`, including duplicated server startup, missing routers, missing router handlers, invalid keepalive configuration, invalid client state, closed clients, full send queues, and reconnect exhaustion.

## Development

Use the parent loader for server startup and shutdown in integration code. Keep each router focused on one WebSocket endpoint, and place connection authentication or ID extraction in `ConnIdentifier` rather than inside message handlers.
