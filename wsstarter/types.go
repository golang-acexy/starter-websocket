package wsstarter

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
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
	Path           string         // 路由路径
	UniqueConnId   bool           // 该路由下 连接标识是否强制唯一，若设置为强制唯一，重复链接的标识旧的将被自动关闭
	ConnIdentifier ConnIdentifier // 连接标识 (可用于鉴权并为链接标识唯一id)
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
	allConn        map[string]map[string]*Conn
}

type Conn struct {
	ConnId string // 连接标识Id 应用层分配

	internalConnId string // 内部连接标识唯一
	handlerWrapper *handlerWrapper
	cancel         context.CancelFunc
	conn           *websocket.Conn
	request        *http.Request

	createdAt       time.Time // 创建时间
	lastReceivePing time.Time // 最后一次收到ping
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
	c.handlerWrapper.closeConnByInternalId(c.ConnId, c.internalConnId)
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

func (h *handlerWrapper) getConn(connId, internalConnId string) (*Conn, bool) {
	defer h.mux.Unlock()
	h.mux.Lock()
	cs, flag := h.allConn[connId]
	if flag {
		c, flag := cs[internalConnId]
		return c, flag
	}
	return nil, false
}

func (h *handlerWrapper) saveConn(connId, internalConnId string, conn *Conn) {
	defer h.mux.Unlock()
	h.mux.Lock()
	v, flag := h.allConn[connId]
	if flag {
		v[internalConnId] = conn
	} else {
		h.allConn[connId] = map[string]*Conn{internalConnId: conn}
	}
}

func (h *handlerWrapper) closeConnById(connId string) {
	defer h.mux.Unlock()
	h.mux.Lock()
	c, flag := h.allConn[connId]
	if flag {
		for i, v := range c {
			_ = v.conn.CloseNow()
			v.cancel()
			delete(c, i)
		}
		delete(h.allConn, connId)
		logger.Logrus().Infoln("connection closed:", connId)
	}
}
func (h *handlerWrapper) closeConnByInternalId(connId, internalConnId string) {
	defer h.mux.Unlock()
	h.mux.Lock()
	cs, flag := h.allConn[connId]
	if flag {
		c, flag := cs[internalConnId]
		if flag {
			_ = c.conn.CloseNow()
			c.cancel()
		}
		delete(cs, internalConnId)
		if len(cs) == 0 {
			delete(h.allConn, connId)
		}
		logger.Logrus().Infoln("connection closed:", connId)
	}
}

func (h *handlerWrapper) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	var connId string
	internalConnId := hashing.Sha256Hex(random.UUID() + time.Now().String() + random.RandString(20))
	if h.connIdentifier != nil {
		var err error
		connId, err = h.connIdentifier(&Request{request})
		if err != nil {
			logger.Logrus().WithError(err).Errorln("connIdentifier failed with error:", err)
			writer.WriteHeader(http.StatusForbidden)
			return
		}
	}
	if h.uniqueConnId {
		if connId == "" {
			logger.Logrus().Errorln("uniqueConnId is true but connIdentifier return empty connId")
			writer.WriteHeader(http.StatusInternalServerError)
			h.mux.Unlock()
			return
		}
		if _, ok := h.getConn(connId, internalConnId); ok {
			logger.Logrus().Warningln("uniqueConnId is set true but connId:", connId, "already exists, replace the old conn")
			h.closeConnById(connId)
		}
	}
	ctx, cancel := context.WithCancel(request.Context())
	opt := webSocketConfig.AcceptOptions
	if opt == nil {
		opt = &websocket.AcceptOptions{}
	}

	// 启用默认keepalive规则
	if webSocketConfig.DefaultKeepAliveConfig != nil && webSocketConfig.CustomKeepAliveConfig == nil {
		// 启用了默认的keepalive规则
		opt.OnPingReceived = func(ctx context.Context, payload []byte) bool {
			c, flag := h.getConn(connId, internalConnId)
			if flag {
				c.lastReceivePing = time.Now()
				return true
			}
			return false
		}
		go func() {
			ticker := time.NewTicker(time.Millisecond * 500)
			defer ticker.Stop()
			keepAliveConfig := webSocketConfig.DefaultKeepAliveConfig
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					c, flag := h.getConn(connId, internalConnId)
					if flag {
						var lastReceivePing time.Time
						if c.lastReceivePing.IsZero() {
							lastReceivePing = c.createdAt
						} else {
							lastReceivePing = c.lastReceivePing
						}
						if time.Now().Sub(lastReceivePing) > keepAliveConfig.PingTimeout {
							logger.Logrus().Infoln("connection ping timeout:", connId, keepAliveConfig.PingTimeout)
							h.closeConnByInternalId(connId, internalConnId)
							return
						}
						if keepAliveConfig.MaxConnectTime > 0 {
							if time.Now().Sub(c.createdAt) > keepAliveConfig.MaxConnectTime { // 检查最大时间
								logger.Logrus().Infoln("connection connect too long:", connId, keepAliveConfig.MaxConnectTime)
								h.closeConnByInternalId(connId, internalConnId)
								return
							}
						}
					}
				}
			}
		}()
	}

	conn, err := websocket.Accept(writer, request, opt)
	if err != nil {
		logger.Logrus().WithError(err).Errorln("connId:", connId, "accept failed with error:", err)
		cancel()
		return
	}
	wsConn := &Conn{
		ConnId:         connId,
		internalConnId: internalConnId,
		handlerWrapper: h,
		conn:           conn,
		cancel:         cancel,
		request:        request,
		createdAt:      time.Now(),
	}
	h.saveConn(connId, internalConnId, wsConn)
	defer func() {
		h.closeConnById(connId)
	}()
	for {
		typ, data, readErr := conn.Read(ctx)
		if readErr != nil {
			return
		}
		h.handler(Message{Type: typ, Data: data}, wsConn)
	}
}
