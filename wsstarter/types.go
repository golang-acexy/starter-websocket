package wsstarter

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/acexy/golang-toolkit/crypto/hashing"
	"github.com/acexy/golang-toolkit/logger"
	"github.com/acexy/golang-toolkit/math/random"
	"github.com/acexy/golang-toolkit/util/coll"
	"github.com/coder/websocket"
)

// Message holds a text or binary frame; Data ownership depends on its producer and is not automatically
// copied.
type Message struct {
	// Type selects text or binary framing; callers must supply a supported message type.
	Type websocket.MessageType
	// Data is the payload; do not mutate shared bytes while a send or consumer uses them.
	Data []byte
}

// ToString returns text verbatim or non-text bytes in hexadecimal; the receiver must be non-nil.
func (m *Message) ToString() string {
	if m.Type == websocket.MessageText {
		return string(m.Data)
	}
	return fmt.Sprintf("%x", m.Data)
}

// NewTextMessage allocates a text payload; an empty string produces an empty message.
func NewTextMessage(data string) *Message {
	return &Message{
		Type: websocket.MessageText,
		Data: []byte(data),
	}
}

// NewBinaryMessage wraps data without copying; nil or empty data produces an empty binary message.
func NewBinaryMessage(data []byte) *Message {
	return &Message{
		Type: websocket.MessageBinary,
		Data: data,
	}
}

// Router defines a ServeMux pattern and its connection policy; configure it before starter resolution.
type Router struct {
	// Path is a non-empty ServeMux pattern; duplicate or invalid patterns are rejected at startup.
	Path string
	// UniqueConnId requires a non-empty identity and closes older connections with the same identity on this
	// route.
	UniqueConnId bool
	// ConnIdentifier overrides server identifiers; nil falls back to global then default identification.
	ConnIdentifier ConnIdentifier
	// Handler must be non-nil and handles messages synchronously for each connection.
	Handler Handler
}

// Handler processes a frame in its connection's read loop; different connections may call it concurrently.
// Keep shared application state safe, avoid blocking reads for long periods, and handle
// application failures explicitly; this wrapper does not recover handler panics.
type Handler func(message Message, conn *Conn)

// ConnIdentifier authenticates a handshake and returns an application identity.
// It can run concurrently across handshakes. An error returns HTTP 403; an empty identity
// is allowed only when uniqueness is disabled. Avoid mutating the request concurrently.
type ConnIdentifier func(request *Request) (string, error)

type handlerWrapper struct {
	mux            sync.RWMutex
	uniqueConnId   bool
	connIdentifier ConnIdentifier
	handler        Handler
	allConn        map[string]map[string]*Conn
}

// Conn wraps one server-side transport and its registered identity.
// Send methods delegate concurrent write serialization to the WebSocket library;
// shared payloads still require caller ownership. Use handler-provided instances only.
type Conn struct {
	// ConnId is the application identity; treat it as read-only because cleanup uses this registry key.
	ConnId string

	internalConnId string
	handlerWrapper *handlerWrapper
	cancel         context.CancelFunc
	conn           *websocket.Conn
	request        *http.Request

	createdAt       time.Time
	lastReceivePing atomic.Int64
}

// SendMessage writes a complete frame using a background context with no explicit deadline.
// Concurrent sends are supported; keep payload bytes unchanged until the call returns.
// An open stream writer must close before another write can proceed.
func (c *Conn) SendMessage(message Message) error {
	return c.conn.Write(context.Background(), message.Type, message.Data)
}

// SendMessageCtx writes a complete frame bounded by a non-nil context.
// Concurrent sends are supported; keep payload bytes unchanged until the call returns.
func (c *Conn) SendMessageCtx(ctx context.Context, message Message) error {
	return c.conn.Write(ctx, message.Type, message.Data)
}

// GetTextMessageWriter opens a text stream with no explicit deadline.
// The caller must close it to finish the frame and release other waiting writes.
func (c *Conn) GetTextMessageWriter() (io.WriteCloser, error) {
	return c.GetTextMessageWriterCtx(context.Background())
}

// GetBinaryMessageWriter opens a binary stream with no explicit deadline.
// The caller must close it to finish the frame and release other waiting writes.
func (c *Conn) GetBinaryMessageWriter() (io.WriteCloser, error) {
	return c.GetBinaryMessageWriterCtx(context.Background())
}

// GetTextMessageWriterCtx opens a text stream bounded by a non-nil context.
// Only one message writer is active at a time; close it before requesting another writer.
func (c *Conn) GetTextMessageWriterCtx(ctx context.Context) (io.WriteCloser, error) {
	return c.conn.Writer(ctx, websocket.MessageText)
}

// GetBinaryMessageWriterCtx opens a binary stream bounded by a non-nil context.
// Only one message writer is active at a time; close it before requesting another writer.
func (c *Conn) GetBinaryMessageWriterCtx(ctx context.Context) (io.WriteCloser, error) {
	return c.conn.Writer(ctx, websocket.MessageBinary)
}

// Close removes and immediately closes this registered connection without a close handshake.
// Concurrent or repeated closure is safe; replacement connections with the same identity
// are preserved. ConnId must remain unchanged.
func (c *Conn) Close() {
	c.handlerWrapper.closeConnByInternalId(c.ConnId, c.internalConnId)
}

// Request exposes the HTTP handshake request; use the non-nil wrapper supplied to identification callbacks.
type Request struct {
	// Request is the underlying handshake request; treat shared request data as read-only.
	*http.Request
}

// GetQuery returns the first query value for key, or an empty string when absent.
func (r *Request) GetQuery(key string) string {
	return r.URL.Query().Get(key)
}

// GetHeader returns the first header value for key, or an empty string when absent.
func (r *Request) GetHeader(key string) string {
	return r.Header.Get(key)
}

func (h *handlerWrapper) getConn(connId, internalConnId string) (*Conn, bool) {
	defer h.mux.RUnlock()
	h.mux.RLock()
	cs, flag := h.allConn[connId]
	if flag {
		c, flag := cs[internalConnId]
		return c, flag
	}
	return nil, false
}

// saveConn installs the replacement before returning old connections for closure outside the registry lock.
func (h *handlerWrapper) saveConn(connId, internalConnId string, conn *Conn) []*Conn {
	h.mux.Lock()
	var replaced []*Conn
	if h.uniqueConnId {
		replaced = coll.MapValues(h.allConn[connId])
		h.allConn[connId] = map[string]*Conn{internalConnId: conn}
	} else {
		connections := h.allConn[connId]
		if connections == nil {
			connections = make(map[string]*Conn)
			h.allConn[connId] = connections
		}
		connections[internalConnId] = conn
	}
	h.mux.Unlock()
	return replaced
}

func (h *handlerWrapper) closeConnById(connId string) {
	h.mux.Lock()
	connections := h.allConn[connId]
	delete(h.allConn, connId)
	h.mux.Unlock()
	coll.MapForEachAll(connections, func(_ string, conn *Conn) {
		conn.closeNow()
	})
	if len(connections) > 0 {
		logger.Logrus().Infoln("connection closed:", connId)
	}
}

// closeConnByInternalId removes only this internal ID so old cleanup cannot delete a replacement.
func (h *handlerWrapper) closeConnByInternalId(connId, internalConnId string) {
	h.mux.Lock()
	connections := h.allConn[connId]
	conn := connections[internalConnId]
	if conn != nil {
		delete(connections, internalConnId)
		if len(connections) == 0 {
			delete(h.allConn, connId)
		}
	}
	h.mux.Unlock()
	if conn != nil {
		conn.closeNow()
		logger.Logrus().Infoln("connection closed:", connId)
	}
}

func (h *handlerWrapper) closeAllConnections() {
	h.mux.Lock()
	groups := coll.MapValues(h.allConn)
	h.allConn = make(map[string]map[string]*Conn)
	h.mux.Unlock()
	connections := coll.SliceFlat(coll.SliceCollect(groups, func(group map[string]*Conn) []*Conn {
		return coll.MapValues(group)
	}))
	coll.SliceForEachAll(connections, func(conn *Conn) {
		conn.closeNow()
	})
}

func (c *Conn) closeNow() {
	_ = c.conn.CloseNow()
	c.cancel()
}

// ServeHTTP authenticates and upgrades a handshake, then dispatches frames until transport closure.
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
			return
		}
	}
	ctx, cancel := context.WithCancel(request.Context())
	config := currentWebsocketConfig()
	if config == nil {
		writer.WriteHeader(http.StatusServiceUnavailable)
		cancel()
		return
	}
	opt := websocket.AcceptOptions{}
	if config.AcceptOptions != nil {
		opt = *config.AcceptOptions
	}

	// Install a per-handshake ping callback without changing shared accept options.
	if config.DefaultKeepAliveConfig != nil {
		opt.OnPingReceived = func(ctx context.Context, payload []byte) bool {
			c, flag := h.getConn(connId, internalConnId)
			if flag {
				c.lastReceivePing.Store(time.Now().UnixNano())
				return true
			}
			return false
		}
		go func() {
			ticker := time.NewTicker(time.Millisecond * 500)
			defer ticker.Stop()
			keepAliveConfig := config.DefaultKeepAliveConfig
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					c, flag := h.getConn(connId, internalConnId)
					if flag {
						lastReceivePing := c.lastReceivePing.Load()
						if lastReceivePing == 0 {
							lastReceivePing = c.createdAt.UnixNano()
						}
						if time.Since(time.Unix(0, lastReceivePing)) > keepAliveConfig.PingTimeout {
							logger.Logrus().Infoln("connection ping timeout:", connId, keepAliveConfig.PingTimeout)
							h.closeConnByInternalId(connId, internalConnId)
							return
						}
						if keepAliveConfig.MaxConnectTime > 0 {
							if time.Since(c.createdAt) > keepAliveConfig.MaxConnectTime {
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

	conn, err := websocket.Accept(writer, request, &opt)
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
	replacedConnections := h.saveConn(connId, internalConnId, wsConn)
	if len(replacedConnections) > 0 {
		logger.Logrus().Warningln("uniqueConnId is set true but connId:", connId, "already exists, replace the old conn")
		coll.SliceForEachAll(replacedConnections, func(replaced *Conn) {
			replaced.closeNow()
		})
	}
	defer func() {
		h.closeConnByInternalId(connId, internalConnId)
	}()
	for {
		typ, data, readErr := conn.Read(ctx)
		if readErr != nil {
			return
		}
		h.handler(Message{Type: typ, Data: data}, wsConn)
	}
}
