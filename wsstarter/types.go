package wsstarter

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/acexy/golang-toolkit/crypto/hashing"
	"github.com/acexy/golang-toolkit/logger"
	"github.com/acexy/golang-toolkit/math/random"
	"github.com/coder/websocket"
)

// Message Websocket 数据
type Message struct {
	Type websocket.MessageType
	Data []byte
}

func (m *Message) ToString() string {
	if m.Type == websocket.MessageText {
		return string(m.Data)
	}
	return fmt.Sprintf("%x", m.Data)
}

// NewTextMessage 创建一个文本数据
func NewTextMessage(data string) *Message {
	return &Message{
		Type: websocket.MessageText,
		Data: []byte(data),
	}
}

// NewBinaryMessage 创建一个二进制数据
func NewBinaryMessage(data []byte) *Message {
	return &Message{
		Type: websocket.MessageBinary,
		Data: data,
	}
}

// Router WS路由
type Router struct {
	Path       string         // 路由路径
	Identifier ConnIdentifier // 连接标识 (可用于做鉴权，GlobalIdentifier会覆盖为nil的设置)
	Handler    Handler        // 路由处理函数
}

type Handler func(message Message, conn *Conn)

// ConnIdentifier 连接标识 为链接分配指定的标识
type ConnIdentifier func(request *Request) (string, error)

type handlerWrapper struct {
	identifier ConnIdentifier
	handler    Handler
}

type Conn struct {
	ConnId  string
	conn    *websocket.Conn
	request *http.Request
}

// SendMessage 发送数据(适用于简短消息)
func (c *Conn) SendMessage(message Message) error {
	return c.conn.Write(context.Background(), message.Type, message.Data)
}

// SendMessageCtx 发送数据(适用于简短消息)
func (c *Conn) SendMessageCtx(ctx context.Context, message Message) error {
	return c.conn.Write(ctx, message.Type, message.Data)
}

// SendStreamMessage 创建一个流式数据发送器
func (c *Conn) SendStreamMessage(ctx context.Context, messageType websocket.MessageType) (io.WriteCloser, error) {
	return c.conn.Writer(ctx, messageType)
}

func (c *Conn) Close() {
	_ = c.conn.CloseNow()
}

// Request 请求包裹
type Request struct {
	*http.Request
}

// GetQuery 获取请求参数
func (r *Request) GetQuery(key string) string {
	return r.URL.Query().Get(key)
}

// GetHeader 获取请求头
func (r *Request) GetHeader(key string) string {
	return r.Header.Get(key)
}

func (h *handlerWrapper) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	var connId string
	if h.identifier != nil {
		var err error
		connId, err = h.identifier(&Request{request})
		if err != nil {
			logger.Logrus().WithError(err).Errorln("identifier failed with error:", err)
			writer.WriteHeader(403)
			return
		}
		if connId == "" {
			connId = hashing.Sha256Hex(random.UUID() + time.Now().String())
		}
	} else {
		connId = hashing.Sha256Hex(random.UUID() + time.Now().String())
	}
	conn, err := websocket.Accept(writer, request, webSocketConfig.AcceptOptions)
	if err != nil {
		logger.Logrus().WithError(err).Errorln("accept failed with error:", err)
		return
	}
	ctx, cancel := context.WithCancel(request.Context())
	defer func() {
		cancel()
		_ = conn.CloseNow()
	}()
	for {
		typ, data, readErr := conn.Read(ctx)
		if readErr != nil {
			return
		}
		h.handler(Message{
			Type: typ,
			Data: data,
		}, &Conn{
			ConnId:  connId,
			conn:    conn,
			request: request,
		})
	}
}
