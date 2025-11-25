package wsstarter

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"

	"github.com/acexy/golang-toolkit/logger"
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
	Path           string         // 路由路径
	UniqueConnId   bool           // 该路由下 连接标识是否唯一
	ConnIdentifier ConnIdentifier // 连接标识 (可用于做鉴权，GlobalIdentifier会覆盖为nil的设置)
	Handler        Handler        // 路由处理函数
}

// Handler 消息处理器
type Handler func(message Message, conn *Conn)

// ConnIdentifier 连接鉴权并返回链接标识Id
type ConnIdentifier func(request *Request) (string, error)

type handlerWrapper struct {
	mux            sync.Mutex
	uniqueConnId   bool
	connIdentifier ConnIdentifier
	handler        Handler
	allConn        map[string]*Conn
}

type Conn struct {
	ConnId  string
	cancel  context.CancelFunc
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

// GetTextMessageWriter 获取文本发送写入流
func (c *Conn) GetTextMessageWriter() (io.WriteCloser, error) {
	return c.GetTextMessageWriterCtx(context.Background())
}

// GetBinaryMessageWriter 获取byte发送写入流
func (c *Conn) GetBinaryMessageWriter() (io.WriteCloser, error) {
	return c.GetBinaryMessageWriterCtx(context.Background())
}

// GetTextMessageWriterCtx 获取文本发送写入流
func (c *Conn) GetTextMessageWriterCtx(ctx context.Context) (io.WriteCloser, error) {
	return c.conn.Writer(ctx, websocket.MessageText)
}

// GetBinaryMessageWriterCtx 获取byte发送写入流
func (c *Conn) GetBinaryMessageWriterCtx(ctx context.Context) (io.WriteCloser, error) {
	return c.conn.Writer(ctx, websocket.MessageBinary)
}

func (c *Conn) Close() {
	_ = c.conn.CloseNow()
	c.cancel()
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
	if h.connIdentifier != nil {
		var err error
		connId, err = h.connIdentifier(&Request{request})
		if err != nil {
			logger.Logrus().WithError(err).Errorln("connIdentifier failed with error:", err)
			writer.WriteHeader(http.StatusForbidden)
			return
		}
	}
	h.mux.Lock()
	if h.uniqueConnId {
		if connId == "" {
			logger.Logrus().Errorln("uniqueConnId is true but connIdentifier return empty connId")
			writer.WriteHeader(http.StatusInternalServerError)
			h.mux.Unlock()
			return
		}
		if c, ok := h.allConn[connId]; ok {
			logger.Logrus().Warningln("uniqueConnId is true but connId:", connId, "already exists replace old conn")
			c.Close()
		}
	}
	ctx, cancel := context.WithCancel(request.Context())
	conn, err := websocket.Accept(writer, request, webSocketConfig.AcceptOptions)
	wsConn := &Conn{
		ConnId:  connId,
		conn:    conn,
		cancel:  cancel,
		request: request,
	}
	h.allConn[connId] = wsConn
	h.mux.Unlock()
	if err != nil {
		logger.Logrus().WithError(err).Errorln("connId:", connId, "accept failed with error:", err)
		return
	}
	defer func(c *websocket.Conn) {
		cancel()
		_ = c.CloseNow()
		logger.Logrus().Infoln("connection closed:", connId)
		delete(h.allConn, connId)
	}(conn)
	for {
		typ, data, readErr := conn.Read(ctx)
		if readErr != nil {
			return
		}
		h.handler(Message{
			Type: typ,
			Data: data,
		}, wsConn)
	}
}
