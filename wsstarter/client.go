package wsstarter

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/acexy/golang-toolkit/logger"
	"github.com/coder/websocket"
)

type WSData struct {
	Type websocket.MessageType
	Data []byte
}

func (w *WSData) ToString() string {
	if w.Type == websocket.MessageText {
		return string(w.Data)
	}
	return fmt.Sprintf("%x", w.Data)
}

// ConnectionState 连接状态枚举
type ConnectionState int32

const (
	StateDisconnected ConnectionState = iota
	StateConnecting
	StateConnected
	StateReconnecting
	StateClosed
)

type WSClient struct {
	url    string
	conn   *websocket.Conn
	cancel context.CancelFunc
	ctx    context.Context
	opts   *websocket.DialOptions

	// 连接状态管理
	state    atomic.Value // ConnectionState
	stateMux sync.RWMutex

	// 重连配置
	maxReconnectAttempts int
	reconnectInterval    time.Duration

	// 心跳配置
	heartbeatInterval time.Duration
	heartbeatPingData string
	heartbeatPongData string
	heartbeatCancel   context.CancelFunc

	// 数据通道
	blockReceive bool
	receiveChan  chan *WSData

	// 发送队列
	blockSender bool
	sendChan    chan *WSData
	sendMux     sync.Mutex

	// 回调函数
	onConnect    func()
	onDisconnect func(error)
	onError      func(error)

	// 优雅关闭
	closeOnce sync.Once
}

// WSClientConfig 配置结构
type WSClientConfig struct {
	URL                  string
	DialOptions          *websocket.DialOptions
	MaxReconnectAttempts int
	ReconnectInterval    time.Duration
	ChanBufferLen        int
	WorkerCount          int
	SendChanBufferLen    int

	// 回调函数
	OnConnect    func()
	OnDisconnect func(error)
	OnError      func(error)
}

func NewWSClient(ctx context.Context, config WSClientConfig) *WSClient {
	ctx, cancel := context.WithCancel(ctx)

	// 设置默认值
	if config.MaxReconnectAttempts == 0 {
		config.MaxReconnectAttempts = 5
	}
	if config.ReconnectInterval == 0 {
		config.ReconnectInterval = time.Second * 2
	}
	if config.ChanBufferLen == 0 {
		config.ChanBufferLen = 100
	}
	if config.WorkerCount == 0 {
		config.WorkerCount = 1
	}
	if config.SendChanBufferLen == 0 {
		config.SendChanBufferLen = 100
	}

	client := &WSClient{
		ctx:                  ctx,
		cancel:               cancel,
		url:                  config.URL,
		opts:                 config.DialOptions,
		maxReconnectAttempts: config.MaxReconnectAttempts,
		reconnectInterval:    config.ReconnectInterval,
		receiveChan:          make(chan *WSData, config.ChanBufferLen),
		sendChan:             make(chan *WSData, config.SendChanBufferLen),
		onConnect:            config.OnConnect,
		onDisconnect:         config.OnDisconnect,
		onError:              config.OnError,
	}

	client.setState(StateDisconnected)
	return client
}

// setState 设置连接状态
func (c *WSClient) setState(state ConnectionState) {
	c.state.Store(state)
}

// GetState 获取连接状态
func (c *WSClient) GetState() ConnectionState {
	return c.state.Load().(ConnectionState)
}

// IsConnected 检查是否已连接
func (c *WSClient) IsConnected() bool {
	return c.GetState() == StateConnected
}

// SetHeartbeat 设置心跳 该函数需要在Connect函数之前注册
// pongData 消息处理器会自动忽略该消息，不会向业务层发送该数据，如果需要，则pongData不设置
func (c *WSClient) SetHeartbeat(interval time.Duration, pingData, pongData string) {
	if c.IsConnected() {
		logger.Logrus().Warningln("heartbeat cannot be set after the connection is established")
		return
	}
	c.heartbeatInterval = interval
	c.heartbeatPingData = pingData
	c.heartbeatPongData = pongData
}

// startHeartbeat 启动心跳
func (c *WSClient) startHeartbeat() {
	if c.heartbeatInterval == 0 {
		return
	}
	// 停止之前的心跳
	c.stopHeartbeat()
	heartbeatCtx, cancel := context.WithCancel(c.ctx)
	c.heartbeatCancel = cancel
	ticker := time.NewTicker(c.heartbeatInterval)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if c.IsConnected() {
					if c.heartbeatPingData != "" {
						_ = c.Send(websocket.MessageText, []byte(c.heartbeatPingData))
					} else {
						_ = c.conn.Ping(context.Background())
					}
				}
			case <-heartbeatCtx.Done():
				logger.Logrus().Traceln("websocket client heartbeat exit")
				return
			}
		}
	}()
}

// stopHeartbeat 停止心跳
func (c *WSClient) stopHeartbeat() {
	if c.heartbeatCancel != nil {
		c.heartbeatCancel()
		c.heartbeatCancel = nil
	}
}

// Connect 连接到 WebSocket 服务器
func (c *WSClient) Connect() (<-chan *WSData, error) {
	if c.GetState() != StateDisconnected {
		return nil, errors.New("client is not in disconnected state")
	}
	c.setState(StateConnecting)
	// 建立连接
	if err := c.dial(); err != nil {
		c.setState(StateDisconnected)
		return nil, fmt.Errorf("failed to connect: %w", err)
	}
	// 启动消息处理协程
	c.startMessageHandlers()
	// 启动发送协程
	c.startSender()
	// 启动心跳
	c.startHeartbeat()
	c.setState(StateConnected)
	if c.onConnect != nil {
		c.onConnect()
	}
	logger.Logrus().Traceln("websocket client connected successfully")
	return c.receiveChan, nil
}

// dial 建立 WebSocket 连接
func (c *WSClient) dial() error {
	conn, _, err := websocket.Dial(c.ctx, c.url, c.opts)
	if err != nil {
		return err
	}

	c.stateMux.Lock()
	c.conn = conn
	c.stateMux.Unlock()

	return nil
}

// startMessageHandlers 启动消息处理协程
func (c *WSClient) startMessageHandlers() {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Logrus().Errorf("websocket message handler panic: %v", r)
				// 发生panic时也要触发重连
				c.handleConnectionError(fmt.Errorf("message handler panic: %v", r))
			}
		}()
		for {
			select {
			case <-c.ctx.Done():
				logger.Logrus().Traceln("websocket client message handler exit")
				return
			default:
				state := c.GetState()
				if state != StateConnected {
					if state == StateClosed {
						return
					}
					time.Sleep(time.Millisecond * 100)
					continue
				}

				c.stateMux.RLock()
				conn := c.conn
				c.stateMux.RUnlock()

				if conn == nil {
					logger.Logrus().Debugln("connection is nil, triggering reconnect")
					c.handleConnectionError(errors.New("connection is nil"))
					return
				}

				readCtx, cancel := context.WithTimeout(c.ctx, time.Second*30)
				messageType, data, err := conn.Read(readCtx)
				cancel()
				if c.heartbeatPongData != "" && messageType == websocket.MessageText && string(data) == c.heartbeatPongData {
					continue
				}
				if err != nil {
					logger.Logrus().Debugf("websocket read error: %v", err)
					c.handleConnectionError(err)
					return
				} else {
					if c.blockReceive {
						c.receiveChan <- &WSData{Type: messageType, Data: data}
					} else {
						select {
						case c.receiveChan <- &WSData{Type: messageType, Data: data}:
						case <-c.ctx.Done():
							return
						default:
							logger.Logrus().Warnln("websocket data channel is full, dropping message")
						}
					}
				}
			}
		}
	}()
}

// startSender 启动发送协程
func (c *WSClient) startSender() {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Logrus().Errorf("websocket sender panic: %v", r)
				c.handleConnectionError(fmt.Errorf("sender panic: %v", r))
			}
		}()

		for {
			select {
			case data := <-c.sendChan:
				if !c.IsConnected() {
					logger.Logrus().Warningln("not connected, dropping message")
					continue
				}

				c.stateMux.RLock()
				conn := c.conn
				c.stateMux.RUnlock()

				if conn == nil {
					logger.Logrus().Warningln("connection is nil in sender")
					continue
				}

				// 设置写入超时
				writeCtx, cancel := context.WithTimeout(c.ctx, time.Second*10)
				err := conn.Write(writeCtx, data.Type, data.Data)
				cancel()
				if err != nil {
					logger.Logrus().Debugf("websocket write error: %v", err)
					if c.onError != nil {
						c.onError(err)
					}
					// 写入失败可能意味着连接有问题，触发重连检查
					go c.handleConnectionError(err)
				}
			case <-c.ctx.Done():
				logger.Logrus().Traceln("websocket client sender exit")
				return
			}
		}
	}()
}

// Send 发送消息
func (c *WSClient) Send(messageType websocket.MessageType, data []byte) error {
	if !c.IsConnected() {
		return errors.New("client is not connected")
	}

	if c.blockSender {
		c.sendChan <- &WSData{Type: messageType, Data: data}
		return nil
	} else {
		select {
		case c.sendChan <- &WSData{Type: messageType, Data: data}:
			return nil
		case <-c.ctx.Done():
			return errors.New("client is closing")
		default:
			return errors.New("send channel is full")
		}
	}
}

// SendText 发送文本消息
func (c *WSClient) SendText(text string) error {
	return c.Send(websocket.MessageText, []byte(text))
}

// SendBinary 发送二进制消息
func (c *WSClient) SendBinary(data []byte) error {
	return c.Send(websocket.MessageBinary, data)
}

// handleConnectionError 处理连接错误
func (c *WSClient) handleConnectionError(err error) {
	currentState := c.GetState()
	if currentState == StateClosed || currentState == StateReconnecting {
		return
	}
	logger.Logrus().Debugln("websocket connection error: %v", err)
	// 判断是否需要重连
	if c.shouldReconnect(err) {
		c.reconnect()
	}
}

// shouldReconnect 判断是否应该重连
func (c *WSClient) shouldReconnect(err error) bool {
	closeStatus := websocket.CloseStatus(err)
	switch closeStatus {
	case websocket.StatusNormalClosure, websocket.StatusGoingAway:
		return false
	default:
		return true
	}
}

// reconnect 重连逻辑
func (c *WSClient) reconnect() {
	currentState := c.GetState()
	if currentState == StateClosed || currentState == StateReconnecting {
		return
	}

	c.setState(StateReconnecting)
	logger.Logrus().Warningln("starting websocket reconnection process")

	go func() {
		backoffDelay := c.reconnectInterval
		maxBackoff := time.Minute * 2

		for attempt := 1; attempt <= c.maxReconnectAttempts; attempt++ {
			if c.GetState() == StateClosed {
				logger.Logrus().Warningln("reconnect cancelled: client closed")
				return
			}

			logger.Logrus().Warningln("websocket reconnect attempt: %d/%d", attempt, c.maxReconnectAttempts)

			// 确保旧连接完全关闭
			c.stateMux.Lock()
			if c.conn != nil {
				_ = c.conn.CloseNow()
				c.conn = nil
			}
			c.stateMux.Unlock()

			// 等待一段时间再重连，使用指数退避
			if attempt > 1 {
				logger.Logrus().Warningln("waiting %v before reconnect attempt %d", backoffDelay, attempt)
				select {
				case <-time.After(backoffDelay):
				case <-c.ctx.Done():
					return
				}

				// 指数退避，但不超过最大值
				backoffDelay *= 2
				if backoffDelay > maxBackoff {
					backoffDelay = maxBackoff
				}
			}

			// 尝试重新建立连接
			if err := c.dial(); err != nil {
				logger.Logrus().Debugf("reconnect attempt %d failed: %v", attempt, err)
				continue
			}

			// 重连成功
			c.setState(StateConnected)

			// 重新启动消息处理和心跳
			c.startMessageHandlers()
			c.startHeartbeat()

			if c.onConnect != nil {
				c.onConnect()
			}

			logger.Logrus().Infoln("websocket reconnect successful")
			return
		}

		// 重连失败
		logger.Logrus().Errorln("websocket reconnect failed after all attempts")
		c.setState(StateDisconnected)

		if c.onError != nil {
			c.onError(errors.New("reconnection failed after all attempts"))
		}
	}()
}

// Close 关闭连接
func (c *WSClient) Close() error {
	var err error
	c.closeOnce.Do(func() {
		c.setState(StateClosed)
		c.stopHeartbeat()

		c.stateMux.Lock()
		if c.conn != nil {
			err = c.conn.Close(websocket.StatusNormalClosure, "client closing")
		}
		c.stateMux.Unlock()
		c.cancel()
		// 关闭通道
		close(c.receiveChan)
		close(c.sendChan)
		logger.Logrus().Traceln("websocket client closed")
	})
	return err
}
