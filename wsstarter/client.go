package wsstarter

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/acexy/golang-toolkit/logger"
	"github.com/acexy/golang-toolkit/util/str"
	"github.com/coder/websocket"
)

// ConnectionState 连接状态枚举
type ConnectionState int32

const defaultDataChanLength = 500

const (
	StateWaitToConnect ConnectionState = iota
	StateDisconnected
	StateConnecting
	StateConnected
	StateReconnecting
	StateClosed
)

type WSClient struct {
	url string

	httpProxyFn func() string
	httpProxy   string

	conn     *websocket.Conn
	response *http.Response
	cancel   context.CancelFunc
	ctx      context.Context
	opts     *websocket.DialOptions

	// 连接状态管理
	state   atomic.Int32
	connMux sync.RWMutex

	// 重连配置
	maxReconnectAttempts int
	reconnectInterval    time.Duration
	disableReconnect     bool
	forceReconnect       bool

	readMaxBytesLimit int64

	// 数据通道
	blockReceive bool
	receiveChan  chan *Message

	// 发送队列
	blockSender bool
	sendChan    chan *Message
	sendMux     sync.Mutex

	// 回调函数
	onConnected    func()
	onDisconnected func(error)
	onError        func(error)
	onClosed       func(error) // 客户端关闭时的一次性回调

	// 优雅关闭
	closeOnce   sync.Once
	closeResult error

	// 用于跟踪各个协程的完成状态
	workerWg      sync.WaitGroup
	workerMux     sync.Mutex
	workersClosed bool
}

// WSClientConfig 配置结构
type WSClientConfig struct {
	URL            string
	HttpProxyURLFn func() string // 代理构造函数 权重高于HttpProxyURL
	HttpProxyURL   string
	DialOptions    *websocket.DialOptions

	DisableReconnect     bool          // 禁用重连 (权重大于ForceReconnect)
	ForceReconnect       bool          // 是否强制重连 只要监测到连接状态异常，就会无限尝试重连
	MaxReconnectAttempts int           // 默认重连3次
	ReconnectInterval    time.Duration // 默认2秒，指数级自动增加等待时间最长1m

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
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	if config.MaxReconnectAttempts == 0 {
		config.MaxReconnectAttempts = 3
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
		httpProxyFn:          config.HttpProxyURLFn,
		opts:                 cloneDialOptions(config.DialOptions),
		maxReconnectAttempts: config.MaxReconnectAttempts,
		disableReconnect:     config.DisableReconnect,
		reconnectInterval:    config.ReconnectInterval,
		forceReconnect:       config.ForceReconnect,
		receiveChan:          make(chan *Message, config.ReceiveChanBufferLen),
		sendChan:             make(chan *Message, config.SendChanBufferLen),
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
	client.workerWg.Add(1)
	go client.contextMonitor()
	return client
}

// contextMonitor 监听context取消事件，执行优雅关闭
func (c *WSClient) contextMonitor() {
	<-c.ctx.Done()
	logger.Logrus().Traceln("context cancelled, initiating graceful shutdown")
	c.workerWg.Done()
	_ = c.Close()
}

func (c *WSClient) addWorker() bool {
	c.workerMux.Lock()
	defer c.workerMux.Unlock()
	if c.workersClosed {
		return false
	}
	c.workerWg.Add(1)
	return true
}

// setState 设置连接状态
func (c *WSClient) setState(state ConnectionState) {
	c.state.Store(int32(state))
}

// GetState 获取连接状态
func (c *WSClient) GetState() ConnectionState {
	return ConnectionState(c.state.Load())
}

func (c *WSClient) transitionState(from, to ConnectionState) bool {
	return c.state.CompareAndSwap(int32(from), int32(to))
}

// IsConnected 检查是否已连接
func (c *WSClient) IsConnected() bool {
	return c.GetState() == StateConnected
}

// Connect 连接到 WebSocket 服务器
func (c *WSClient) Connect() (<-chan *Message, error) {
	if c.url == "" {
		return nil, ErrClientURLMissing
	}
	if c.maxReconnectAttempts < 0 {
		return nil, ErrReconnectAttemptsInvalid
	}
	if c.reconnectInterval < 0 {
		return nil, ErrReconnectIntervalInvalid
	}
	if !c.transitionState(StateWaitToConnect, StateConnecting) {
		return nil, ErrClientNotWaitToConnect
	}
	// 建立连接
	if err := c.dial(); err != nil {
		c.transitionState(StateConnecting, StateDisconnected)
		return nil, fmt.Errorf("failed to connect: %w", err)
	}
	if !c.transitionState(StateConnecting, StateConnected) {
		c.closeCurrentConnection()
		return nil, ErrClientClosing
	}
	// 启动消息处理协程
	c.startMessageHandler()
	// 启动发送协程
	c.startMessageSender()
	if c.onConnected != nil {
		c.onConnected()
	}
	logger.Logrus().Traceln("websocket client connected successfully")
	return c.receiveChan, nil
}

// dial 建立 WebSocket 连接
func (c *WSClient) dial() error {
	dialOptions := websocket.DialOptions{}
	if c.opts != nil {
		dialOptions = *c.opts
	}
	if c.httpProxyFn != nil || c.httpProxy != "" {
		var proxyURL *url.URL
		var err error
		if c.httpProxyFn != nil {
			proxyUrl := c.httpProxyFn()
			if str.HasText(proxyUrl) {
				proxyURL, err = url.Parse(proxyUrl)
				if err != nil {
					return fmt.Errorf("invalid proxy address: %s %w", proxyUrl, err)
				}
			}
		} else if c.httpProxy != "" {
			proxyURL, err = url.Parse(c.httpProxy)
			if err != nil {
				return fmt.Errorf("invalid proxy address: %s %w", c.httpProxy, err)
			}
		}
		if proxyURL != nil {
			transport := &http.Transport{
				Proxy: http.ProxyURL(proxyURL),
			}
			dialOptions.HTTPClient = &http.Client{Transport: transport}
		}
	}
	conn, response, err := websocket.Dial(c.ctx, c.url, &dialOptions)
	if err != nil {
		return err
	}
	if c.readMaxBytesLimit > 0 {
		conn.SetReadLimit(c.readMaxBytesLimit)
	}
	c.connMux.Lock()
	c.response = response
	c.conn = conn
	c.connMux.Unlock()
	return nil
}

// startMessageHandler 启动消息处理协程
func (c *WSClient) startMessageHandler() {
	if !c.addWorker() {
		return
	}
	go func() {
		defer func() {
			logger.Logrus().Traceln("websocket message handlerWrapper exit")
			c.workerWg.Done()
		}()
		defer func() {
			if r := recover(); r != nil {
				logger.Logrus().Errorf("websocket message handlerWrapper panic: %v", r)
				// 发生panic时也要触发重连
				c.handleConnectionError(fmt.Errorf("message handlerWrapper panic: %v", r))
			}
		}()
		for {
			select {
			case <-c.ctx.Done():
				logger.Logrus().Warningln("websocket client message handlerWrapper exit due to context cancellation")
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
				c.connMux.RLock()
				conn := c.conn
				c.connMux.RUnlock()
				if conn == nil {
					logger.Logrus().Warningln("connection is nil, triggering reconnect")
					c.handleConnectionError(ErrConnectionNil)
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
					case c.receiveChan <- &Message{Type: messageType, Data: data}:
					case <-c.ctx.Done():
						return
					}
				} else {
					select {
					case c.receiveChan <- &Message{Type: messageType, Data: data}:
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
	if !c.addWorker() {
		return
	}
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
				c.connMux.RLock()
				conn := c.conn
				c.connMux.RUnlock()

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
	c.sendMux.Lock()
	defer c.sendMux.Unlock()
	if !c.IsConnected() {
		return ErrClientNotConnected
	}
	if c.blockSender {
		select {
		case c.sendChan <- &Message{Type: messageType, Data: data}:
			return nil
		case <-c.ctx.Done():
			return ErrClientClosing
		}
	} else {
		select {
		case c.sendChan <- &Message{Type: messageType, Data: data}:
			return nil
		case <-c.ctx.Done():
			return ErrClientClosing
		default:
			return ErrSendChannelFull
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

	for {
		currentState := c.GetState()
		if currentState == StateClosed || currentState == StateReconnecting || currentState == StateDisconnected {
			return
		}
		nextState := StateDisconnected
		if c.shouldReconnect(err) {
			nextState = StateReconnecting
		}
		if !c.transitionState(currentState, nextState) {
			continue
		}
		logger.Logrus().Warningf("websocket connection error: %v", err)
		c.closeCurrentConnection()
		if c.onDisconnected != nil {
			c.onDisconnected(err)
		}
		if nextState == StateReconnecting {
			c.reconnect()
		}
		return
	}
}

// shouldReconnect 判断是否应该重连
func (c *WSClient) shouldReconnect(err error) bool {
	if c.disableReconnect {
		return false
	}
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
	if c.GetState() != StateReconnecting {
		return
	}
	logger.Logrus().Debugln("starting websocket reconnection process")
	if !c.addWorker() {
		return
	}
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
			// 等待一段时间再重连，使用指数退避
			if attempt > 1 {
				logger.Logrus().Debugf("waiting %v before reconnect attempt %d", backoffDelay, attempt)
				if c.forceReconnect {
					logger.Logrus().Debugf("waiting %v before reconnect attempt: forced reconnect", backoffDelay)
				} else {
					logger.Logrus().Debugf("waiting %v before reconnect attempt: %d", backoffDelay, attempt)
				}
				timer := time.NewTimer(backoffDelay)
				select {
				case <-timer.C:
				case <-c.ctx.Done():
					timer.Stop()
					logger.Logrus().Warningln("reconnect cancelled: context cancelled during backoff")
					return
				}
				timer.Stop()
				// 指数退避，但不超过最大值
				backoffDelay *= 2
				if backoffDelay > maxBackoff {
					backoffDelay = maxBackoff
				}
			}

			// 尝试重新建立连接
			if err := c.dial(); err != nil {
				logger.Logrus().Warningf("reconnect attempt %d failed: %v", attempt, err)
				continue
			}
			if !c.transitionState(StateReconnecting, StateConnected) {
				c.closeCurrentConnection()
				return
			}
			logger.Logrus().Debugln("websocket reconnect successful")
			// 重连成功
			// 重新启动消息处理和心跳
			c.startMessageHandler()
			if c.onConnected != nil {
				c.onConnected()
			}
			return
		}

		// 重连失败，关闭客户端
		logger.Logrus().Warningln("websocket reconnect failed after all attempts")
		c.transitionState(StateReconnecting, StateDisconnected)
		if c.onError != nil {
			c.onError(ErrReconnectAttemptsExhausted)
		}
		go func() {
			_ = c.Close()
		}()
	}()
}

func (c *WSClient) closeCurrentConnection() {
	c.connMux.Lock()
	conn := c.conn
	c.conn = nil
	c.connMux.Unlock()
	if conn != nil {
		_ = conn.CloseNow()
	}
}

// Close 关闭连接
func (c *WSClient) Close() error {
	return c.CloseWithError(nil)
}

// Ping 发送心跳包
func (c *WSClient) Ping() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()
	return c.PingContext(ctx)
}

// PingContext 使用指定上下文发送心跳包。
func (c *WSClient) PingContext(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if !c.IsConnected() {
		return ErrClientNotConnected
	}
	c.connMux.RLock()
	conn := c.conn
	c.connMux.RUnlock()
	if conn == nil {
		return ErrConnectionNil
	}
	return conn.Ping(ctx)
}

func cloneDialOptions(options *websocket.DialOptions) *websocket.DialOptions {
	if options == nil {
		return nil
	}
	cloned := *options
	cloned.HTTPHeader = options.HTTPHeader.Clone()
	cloned.Subprotocols = append([]string(nil), options.Subprotocols...)
	return &cloned
}

// CloseWithError 带错误信息的关闭连接
func (c *WSClient) CloseWithError(closeErr error) error {
	c.closeOnce.Do(func() {
		logger.Logrus().Traceln("close websocket client ...")
		previousState := ConnectionState(c.state.Swap(int32(StateClosed)))
		c.workerMux.Lock()
		c.workersClosed = true
		c.workerMux.Unlock()
		c.cancel()
		c.connMux.Lock()
		conn := c.conn
		c.conn = nil
		c.connMux.Unlock()
		if conn != nil {
			c.closeResult = conn.Close(websocket.StatusNormalClosure, "client closed")
		}

		// 等待所有工作协程完成
		done := make(chan struct{})
		go func() {
			c.workerWg.Wait()
			logger.Logrus().Traceln("all worker goroutines exited")
			close(done)
		}()

		// 等待所有协程退出，但设置超时防止死锁
		workersStopped := false
		timer := time.NewTimer(time.Second * 5)
		defer timer.Stop()
		select {
		case <-done:
			workersStopped = true
			logger.Logrus().Traceln("all worker goroutines exited")
		case <-timer.C:
			logger.Logrus().Warningln("timeout waiting for worker goroutines to exit")
		}
		if workersStopped {
			close(c.receiveChan)
		}

		if c.onDisconnected != nil && previousState != StateDisconnected && previousState != StateReconnecting && previousState != StateClosed && previousState != StateWaitToConnect {
			c.onDisconnected(closeErr)
		}

		// 最后调用关闭回调（保证只调用一次）
		if c.onClosed != nil {
			c.onClosed(closeErr)
		}
		logger.Logrus().Traceln("websocket client closed successfully")
	})
	return c.closeResult
}
