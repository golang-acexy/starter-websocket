package wsstarter

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
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

var defaultDataChanLength = 500

const (
	StateWaitToConnect ConnectionState = iota
	StateDisconnected                  = iota
	StateConnecting
	StateConnected
	StateReconnecting
	StateClosed
)

type WSClient struct {
	url       string
	httpProxy string
	conn      *websocket.Conn
	cancel    context.CancelFunc
	ctx       context.Context
	opts      *websocket.DialOptions

	// 连接状态管理
	state    atomic.Value // ConnectionState
	stateMux sync.RWMutex

	// 重连配置
	maxReconnectAttempts int
	reconnectInterval    time.Duration
	forceReconnect       bool

	readMaxBytesLimit int64

	// 数据通道
	blockReceive bool
	receiveChan  chan *WSData

	// 发送队列
	blockSender bool
	sendChan    chan *WSData
	sendMux     sync.Mutex

	// 回调函数
	onConnected    func()
	onDisconnected func(error)
	onError        func(error)
	onClosed       func(error) // 客户端关闭时的一次性回调

	// 优雅关闭
	closeOnce sync.Once

	// 用于跟踪各个协程的完成状态
	workerWg sync.WaitGroup
}

// WSClientConfig 配置结构
type WSClientConfig struct {
	URL          string
	HttpProxyURL string
	DialOptions  *websocket.DialOptions

	ForceReconnect       bool // 是否强制重连 只要监测到连接状态异常，就会无限尝试重连
	MaxReconnectAttempts int
	ReconnectInterval    time.Duration

	ReceiveChanBufferLen int   // 接收数据通道缓冲长度 非阻塞式模式生效 默认 500
	SendChanBufferLen    int   // 发送数据通道缓冲长度 非阻塞式模式生效 默认 500
	ReadMaxBytesLimit    int64 // 接收数据最大字节数限制

	BlockReceive bool // 阻塞式接收数据
	BlockSender  bool // 阻塞式发送数据

	// 回调函数
	OnConnected    func()
	OnDisconnected func(error)
	OnError        func(error)
	OnClosed       func(error) // 客户端关闭时的一次性回调，保证只调用一次
}

func NewWSClient(ctx context.Context, config WSClientConfig) *WSClient {
	ctx, cancel := context.WithCancel(ctx)
	if config.MaxReconnectAttempts == 0 {
		config.MaxReconnectAttempts = 5
	}
	if config.ReconnectInterval == 0 {
		config.ReconnectInterval = time.Second * 2
	}
	if config.ReceiveChanBufferLen <= 0 {
		config.ReceiveChanBufferLen = defaultDataChanLength
	}
	if config.SendChanBufferLen <= 0 {
		config.SendChanBufferLen = defaultDataChanLength
	}
	client := &WSClient{
		ctx:                  ctx,
		cancel:               cancel,
		url:                  config.URL,
		httpProxy:            config.HttpProxyURL,
		opts:                 config.DialOptions,
		maxReconnectAttempts: config.MaxReconnectAttempts,
		reconnectInterval:    config.ReconnectInterval,
		forceReconnect:       config.ForceReconnect,
		receiveChan:          make(chan *WSData, config.ReceiveChanBufferLen),
		sendChan:             make(chan *WSData, config.SendChanBufferLen),
		onConnected:          config.OnConnected,
		onDisconnected:       config.OnDisconnected,
		onError:              config.OnError,
		onClosed:             config.OnClosed,
		blockReceive:         config.BlockReceive,
		blockSender:          config.BlockSender,
		readMaxBytesLimit:    config.ReadMaxBytesLimit,
	}
	client.setState(StateWaitToConnect)
	// 启动context监听协程，当context取消时执行优雅关闭
	go client.contextMonitor()
	return client
}

// contextMonitor 监听context取消事件，执行优雅关闭
func (c *WSClient) contextMonitor() {
	c.workerWg.Add(1)
	<-c.ctx.Done()
	logger.Logrus().Traceln("context cancelled, initiating graceful shutdown")
	c.workerWg.Done()
	_ = c.Close()
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

// Connect 连接到 WebSocket 服务器
func (c *WSClient) Connect() (<-chan *WSData, error) {
	if c.GetState() != StateWaitToConnect {
		return nil, errors.New("client is not in wait to connect state")
	}
	c.setState(StateConnecting)
	// 建立连接
	if err := c.dial(); err != nil {
		c.setState(StateDisconnected)
		return nil, fmt.Errorf("failed to connect: %w", err)
	}
	// 启动消息处理协程
	c.startMessageHandler()
	// 启动发送协程
	c.startMessageSender()
	c.setState(StateConnected)
	if c.onConnected != nil {
		c.onConnected()
	}
	logger.Logrus().Traceln("websocket client connected successfully")
	return c.receiveChan, nil
}

// dial 建立 WebSocket 连接
func (c *WSClient) dial() error {
	if c.httpProxy != "" {
		proxyURL, err := url.Parse(c.httpProxy)
		if err != nil {
			return fmt.Errorf("invalid proxy address: %w", err)
		}
		transport := &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
		}
		if c.opts == nil {
			c.opts = &websocket.DialOptions{}
		}
		c.opts.HTTPClient = &http.Client{Transport: transport}
	}
	conn, _, err := websocket.Dial(c.ctx, c.url, c.opts)
	if err != nil {
		return err
	}
	if c.readMaxBytesLimit > 0 {
		conn.SetReadLimit(c.readMaxBytesLimit)
	}
	c.stateMux.Lock()
	c.conn = conn
	c.stateMux.Unlock()
	return nil
}

// startMessageHandler 启动消息处理协程
func (c *WSClient) startMessageHandler() {
	c.workerWg.Add(1)
	go func() {
		defer func() {
			logger.Logrus().Traceln("websocket message handler exit")
			c.workerWg.Done()
		}()
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
				logger.Logrus().Warningln("websocket client message handler exit due to context cancellation")
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
					logger.Logrus().Warningln("connection is nil, triggering reconnect")
					c.handleConnectionError(errors.New("connection is nil"))
					return
				}

				// 移除超时设置，让Read操作阻塞直到有消息或连接断开
				messageType, data, err := conn.Read(c.ctx)
				if err != nil {
					// 检查是否是context取消导致的错误
					if c.ctx.Err() != nil {
						logger.Logrus().Warningln("websocket read interrupted by context cancellation")
						return
					}
					logger.Logrus().Errorf("websocket read error: %v", err)
					c.handleConnectionError(err)
					return
				}
				// 处理普通消息
				if c.blockReceive {
					select {
					case c.receiveChan <- &WSData{Type: messageType, Data: data}:
					case <-c.ctx.Done():
						return
					}
				} else {
					select {
					case c.receiveChan <- &WSData{Type: messageType, Data: data}:
					case <-c.ctx.Done():
						return
					default:
						logger.Logrus().Warnln("websocket receive data channel is full, dropping message", c.url)
					}
				}
			}
		}
	}()
}

// startMessageSender 启动发送协程
func (c *WSClient) startMessageSender() {
	c.workerWg.Add(1)
	go func() {
		defer func() {
			c.workerWg.Done()
			logger.Logrus().Traceln("websocket message sender exit")
		}()
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

				// 设置写入超时（写入超时是合理的）
				writeCtx, cancel := context.WithTimeout(c.ctx, time.Second*10)
				err := conn.Write(writeCtx, data.Type, data.Data)
				cancel()
				if err != nil {
					// 检查是否是context取消导致的错误
					if c.ctx.Err() != nil {
						logger.Logrus().Traceln("websocket write interrupted by context cancellation")
						return
					}
					logger.Logrus().Warningf("websocket write error: %v", err)
					if c.onError != nil {
						c.onError(err)
					}
					// 写入失败可能意味着连接有问题，触发重连检查
					go c.handleConnectionError(err)
				}
			case <-c.ctx.Done():
				logger.Logrus().Traceln("websocket client sender exit due to context cancellation")
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
		select {
		case c.sendChan <- &WSData{Type: messageType, Data: data}:
			return nil
		case <-c.ctx.Done():
			return errors.New("client is closing")
		}
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
	// 检查是否是context取消导致的错误
	if c.ctx.Err() != nil {
		logger.Logrus().Warningln("connection error ignored due to context cancellation")
		return
	}

	currentState := c.GetState()
	if currentState == StateClosed || currentState == StateReconnecting {
		return
	}
	logger.Logrus().Warningf("websocket connection error: %v", err)

	// 调用断开连接回调
	if c.onDisconnected != nil {
		c.onDisconnected(err)
	}

	// 判断是否需要重连
	if c.shouldReconnect(err) {
		c.reconnect()
	}
}

// shouldReconnect 判断是否应该重连
func (c *WSClient) shouldReconnect(err error) bool {
	if c.forceReconnect {
		return true
	}
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
	logger.Logrus().Debugln("starting websocket reconnection process")
	c.workerWg.Add(1)
	go func() {
		defer c.workerWg.Done()
		backoffDelay := c.reconnectInterval
		maxBackoff := time.Minute // 最大间隔不超过一分钟
		var maxReconnectAttempts int
		if c.forceReconnect {
			maxReconnectAttempts = math.MaxInt
		} else {
			maxReconnectAttempts = c.maxReconnectAttempts
		}
		for attempt := 1; attempt <= maxReconnectAttempts; attempt++ {
			// 检查context是否已取消
			if c.ctx.Err() != nil {
				logger.Logrus().Warningln("reconnect cancelled: context cancelled")
				return
			}
			if c.GetState() == StateClosed {
				logger.Logrus().Warningln("reconnect cancelled: client closed")
				return
			}
			if c.forceReconnect {
				logger.Logrus().Debugf("websocket reconnect attempt: forced reconnect")
			} else {
				logger.Logrus().Debugf("websocket reconnect attempt: %d/%d", attempt, c.maxReconnectAttempts)
			}
			// 确保旧连接完全关闭
			c.stateMux.Lock()
			if c.conn != nil {
				_ = c.conn.CloseNow()
				c.conn = nil
			}
			c.stateMux.Unlock()
			// 等待一段时间再重连，使用指数退避
			if attempt > 1 {
				logger.Logrus().Debugf("waiting %v before reconnect attempt %d", backoffDelay, attempt)
				if c.forceReconnect {
					logger.Logrus().Debugf("waiting %v before reconnect attempt: forced reconnect", backoffDelay)
				} else {
					logger.Logrus().Debugf("waiting %v before reconnect attempt: %d", backoffDelay, attempt)
				}
				select {
				case <-time.After(backoffDelay):
				case <-c.ctx.Done():
					logger.Logrus().Warningln("reconnect cancelled: context cancelled during backoff")
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
				logger.Logrus().Warningln("reconnect attempt %d failed: %v", attempt, err)
				continue
			}
			logger.Logrus().Debugf("websocket reconnect successful")
			// 重连成功
			c.setState(StateConnected)
			// 重新启动消息处理和心跳
			c.startMessageHandler()
			if c.onConnected != nil {
				c.onConnected()
			}
			return
		}

		// 重连失败，关闭客户端
		logger.Logrus().Warningln("websocket reconnect failed after all attempts")
		c.setState(StateDisconnected)
		if c.onError != nil {
			c.onError(errors.New("reconnection failed after all attempts"))
		}
		_ = c.Close()
	}()
}

// Close 关闭连接
func (c *WSClient) Close() error {
	return c.CloseWithError(nil)
}

// CloseWithError 带错误信息的关闭连接
func (c *WSClient) CloseWithError(closeErr error) error {
	var err error
	c.closeOnce.Do(func() {
		logger.Logrus().Traceln("close websocket client ...")
		c.setState(StateClosed)
		// 关闭WebSocket连接
		c.stateMux.Lock()
		if c.conn != nil {
			err = c.conn.Close(websocket.StatusNormalClosure, "client closed")
			c.conn = nil
		}
		c.stateMux.Unlock()

		// 取消context，通知所有协程退出
		c.cancel()

		// 关闭通道
		close(c.receiveChan)
		close(c.sendChan)

		// 等待所有工作协程完成
		done := make(chan struct{})
		go func() {
			c.workerWg.Wait()
			logger.Logrus().Traceln("all worker goroutines exited")
			close(done)
		}()

		// 等待所有协程退出，但设置超时防止死锁
		select {
		case <-done:
			logger.Logrus().Traceln("all worker goroutines exited")
		case <-time.After(time.Second * 5):
			logger.Logrus().Warningln("timeout waiting for worker goroutines to exit")
		}

		// 先调用断开连接回调（可能会被多次调用）
		if c.onDisconnected != nil {
			c.onDisconnected(closeErr)
		}

		// 最后调用关闭回调（保证只调用一次）
		if c.onClosed != nil {
			c.onClosed(closeErr)
		}
		logger.Logrus().Traceln("websocket client closed successfully")
	})
	return err
}
