package wsstarter

import (
	"context"
	"errors"
	"fmt"
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

const (
	StateDisconnected ConnectionState = iota
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

	// 心跳配置
	heartbeatInterval        time.Duration
	heartbeatTimeout         time.Duration // 心跳超时时间
	heartbeatPingData        string
	heartbeatPongData        string
	heartbeatCancel          context.CancelFunc
	showHeartbeatTraceLogger bool
	lastPongTime             atomic.Value // time.Time - 最后收到pong的时间

	readLimit int64

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
	URL                  string
	HttpProxyURL         string
	DialOptions          *websocket.DialOptions
	MaxReconnectAttempts int
	ReconnectInterval    time.Duration
	ChanBufferLen        int
	WorkerCount          int
	SendChanBufferLen    int
	ReadLimit            int64

	BlockReceive bool // 阻塞式接收数据
	BlockSender  bool // 阻塞式发送数据

	// 心跳超时配置
	HeartbeatTimeout         time.Duration
	ShowHeartbeatTraceLogger bool

	// 回调函数
	OnConnected    func()
	OnDisconnected func(error)
	OnError        func(error)
	OnClosed       func(error) // 新增：客户端关闭时的一次性回调，保证只调用一次
}

func NewWSClient(ctx context.Context, config WSClientConfig) *WSClient {
	ctx, cancel := context.WithCancel(ctx)
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
	if config.HeartbeatTimeout == 0 {
		config.HeartbeatTimeout = time.Second * 60 // 默认60秒心跳超时
	}
	client := &WSClient{
		ctx:                      ctx,
		cancel:                   cancel,
		url:                      config.URL,
		httpProxy:                config.HttpProxyURL,
		opts:                     config.DialOptions,
		maxReconnectAttempts:     config.MaxReconnectAttempts,
		reconnectInterval:        config.ReconnectInterval,
		heartbeatTimeout:         config.HeartbeatTimeout,
		receiveChan:              make(chan *WSData, config.ChanBufferLen),
		sendChan:                 make(chan *WSData, config.SendChanBufferLen),
		onConnected:              config.OnConnected,
		onDisconnected:           config.OnDisconnected,
		onError:                  config.OnError,
		onClosed:                 config.OnClosed, // 新增
		showHeartbeatTraceLogger: config.ShowHeartbeatTraceLogger,
		blockReceive:             config.BlockReceive,
		blockSender:              config.BlockSender,
		readLimit:                config.ReadLimit,
	}
	client.setState(StateDisconnected)
	client.lastPongTime.Store(time.Now())
	// 启动context监听协程，当context取消时执行优雅关闭
	client.workerWg.Add(1)
	go client.contextMonitor()
	return client
}

// contextMonitor 监听context取消事件，执行优雅关闭
func (c *WSClient) contextMonitor() {
	defer c.workerWg.Done()
	<-c.ctx.Done()
	logger.Logrus().Traceln("context cancelled, initiating graceful shutdown")
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

// SetHeartbeat 设置心跳 该函数需要在Connect函数之前注册
// 有两种心跳模式：
//  1. 自定义心跳：设置pingData和pongData，客户端会发送pingData并期望收到pongData响应
//     收到正确的pongData时会更新最后心跳时间，如果超时未收到则触发重连
//  2. 原生心跳：只设置interval，pingData和pongData留空，使用WebSocket原生ping/pong帧
//     原生ping发送成功时会更新最后心跳时间，如果ping发送失败或超时未更新则触发重连
//
// 无论使用哪种模式，都会启动心跳超时检测，超过heartbeatTimeout时间未更新心跳则重连
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

	// 使用channel来同步第一次心跳发送完成
	firstHeartbeatSent := make(chan struct{})

	// 心跳发送协程
	c.workerWg.Add(1)
	go func() {
		defer c.workerWg.Done()
		ticker := time.NewTicker(c.heartbeatInterval)
		defer ticker.Stop()

		// 立即发送第一次心跳
		if c.IsConnected() {
			if c.heartbeatPingData != "" {
				// 使用自定义心跳消息
				err := c.Send(websocket.MessageText, []byte(c.heartbeatPingData))
				if err != nil {
					logger.Logrus().Warningf("send heartbeat ping failed: %v", err)
					c.handleConnectionError(err)
					close(firstHeartbeatSent) // 即使失败也要关闭channel
					return
				}
				if c.showHeartbeatTraceLogger {
					logger.Logrus().Traceln("first custom heartbeat ping sent")
				}
			} else {
				// 使用WebSocket原生ping
				c.stateMux.RLock()
				conn := c.conn
				c.stateMux.RUnlock()

				if conn != nil {
					pingCtx, pingCancel := context.WithTimeout(c.ctx, time.Second*5)
					err := conn.Ping(pingCtx)
					pingCancel()

					if err != nil {
						logger.Logrus().Tracef("first ping failed: %v", err)
						c.handleConnectionError(err)
						close(firstHeartbeatSent) // 即使失败也要关闭channel
						return
					} else {
						// ping成功发送，更新时间（对于原生心跳，ping成功表示连接正常）
						c.lastPongTime.Store(time.Now())
						if c.showHeartbeatTraceLogger {
							logger.Logrus().Traceln("first native ping sent successfully")
						}
					}
				}
			}
		}

		// 通知第一次心跳已发送
		close(firstHeartbeatSent)

		// 定期发送心跳
		for {
			select {
			case <-ticker.C:
				if c.IsConnected() {
					if c.heartbeatPingData != "" {
						// 使用自定义心跳消息
						err := c.Send(websocket.MessageText, []byte(c.heartbeatPingData))
						if err != nil {
							logger.Logrus().Tracef("send heartbeat ping failed: %v", err)
							c.handleConnectionError(err)
							return
						}
						if c.showHeartbeatTraceLogger {
							logger.Logrus().Traceln("custom heartbeat ping sent")
						}
					} else {
						// 使用WebSocket原生ping
						c.stateMux.RLock()
						conn := c.conn
						c.stateMux.RUnlock()
						if conn != nil {
							pingCtx, pingCancel := context.WithTimeout(c.ctx, time.Second*5)
							err := conn.Ping(pingCtx)
							pingCancel()

							if err != nil {
								logger.Logrus().Tracef("ping failed: %v", err)
								c.handleConnectionError(err)
								return
							} else {
								// ping成功发送，更新时间（对于原生心跳，ping成功表示连接正常）
								c.lastPongTime.Store(time.Now())
								if c.showHeartbeatTraceLogger {
									logger.Logrus().Traceln("native ping sent successfully")
								}
							}
						}
					}
				}
			case <-heartbeatCtx.Done():
				logger.Logrus().Traceln("websocket client heartbeat sender exit")
				return
			}
		}
	}()

	// 启动心跳超时检测协程（等待第一次心跳发送后再开始检测）
	c.workerWg.Add(1)
	go func() {
		defer c.workerWg.Done()
		// 等待第一次心跳发送完成
		select {
		case <-firstHeartbeatSent:
			if c.showHeartbeatTraceLogger {
				logger.Logrus().Traceln("heartbeat monitor starting after first heartbeat sent")
			}
		case <-heartbeatCtx.Done():
			logger.Logrus().Traceln("websocket client heartbeat monitor exit before first heartbeat")
			return
		}
		// 等待一个合理的时间后再开始检测，给心跳响应充足的时间
		// 对于自定义心跳，需要等待pong响应；对于原生心跳，ping成功后就已经更新了时间
		var initialDelay time.Duration
		if c.heartbeatPingData != "" && c.heartbeatPongData != "" {
			// 自定义心跳模式：等待heartbeatTimeout时间，给pong响应留出时间
			initialDelay = c.heartbeatTimeout
		} else {
			// 原生心跳模式：等待一个心跳间隔，因为ping成功就已经更新了时间
			initialDelay = c.heartbeatInterval
		}

		select {
		case <-time.After(initialDelay):
			// 延迟等待结束后，重新设置lastPongTime为当前时间作为检测基准
			// 这样可以避免在延迟期间收到的心跳响应导致的时间基准问题
			c.lastPongTime.Store(time.Now())
			if c.showHeartbeatTraceLogger {
				logger.Logrus().Tracef("heartbeat timeout detection started after %v delay, reset baseline time", initialDelay)
			}
		case <-heartbeatCtx.Done():
			logger.Logrus().Traceln("websocket client heartbeat monitor exit during initial wait")
			return
		}

		// 检测频率为心跳间隔的一半，但不超过30秒，不少于1秒
		checkInterval := c.heartbeatInterval / 2
		if checkInterval > time.Second*30 {
			checkInterval = time.Second * 30
		}
		if checkInterval < time.Second {
			checkInterval = time.Second
		}
		ticker := time.NewTicker(checkInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if c.IsConnected() {
					lastPong := c.lastPongTime.Load().(time.Time)
					timeSinceLastPong := time.Since(lastPong)
					if timeSinceLastPong > c.heartbeatTimeout {
						if c.heartbeatPingData != "" && c.heartbeatPongData != "" {
							logger.Logrus().Warningf("custom heartbeat timeout detected (last pong: %v ago, timeout: %v), triggering reconnect", timeSinceLastPong, c.heartbeatTimeout)
						} else {
							logger.Logrus().Warningf("native heartbeat timeout detected (last ping success: %v ago, timeout: %v), triggering reconnect", timeSinceLastPong, c.heartbeatTimeout)
						}
						c.handleConnectionError(errors.New("heartbeat timeout"))
						return
					}
					if c.showHeartbeatTraceLogger {
						logger.Logrus().Tracef("heartbeat check: last activity %v ago, timeout threshold %v", timeSinceLastPong, c.heartbeatTimeout)
					}
				}
			case <-heartbeatCtx.Done():
				logger.Logrus().Traceln("websocket client heartbeat monitor exit")
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
	if c.readLimit > 0 {
		conn.SetReadLimit(c.readLimit)
	}
	c.stateMux.Lock()
	c.conn = conn
	c.stateMux.Unlock()
	return nil
}

// startMessageHandlers 启动消息处理协程
func (c *WSClient) startMessageHandlers() {
	c.workerWg.Add(1)
	go func() {
		defer c.workerWg.Done()
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

				// 检查是否是自定义心跳pong消息
				if c.heartbeatPongData != "" && messageType == websocket.MessageText && string(data) == c.heartbeatPongData {
					// 更新最后收到pong的时间
					c.lastPongTime.Store(time.Now())
					if c.showHeartbeatTraceLogger {
						logger.Logrus().Traceln("received custom heartbeat pong")
					}
					continue
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
						logger.Logrus().Warnln("websocket receive data channel is full, dropping message")
					}
				}
			}
		}
	}()
}

// startSender 启动发送协程
func (c *WSClient) startSender() {
	c.workerWg.Add(1)
	go func() {
		defer c.workerWg.Done()
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
		logger.Logrus().Traceln("connection error ignored due to context cancellation")
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

	c.workerWg.Add(1)
	go func() {
		defer c.workerWg.Done()
		backoffDelay := c.reconnectInterval
		maxBackoff := time.Minute * 2

		for attempt := 1; attempt <= c.maxReconnectAttempts; attempt++ {
			// 检查context是否已取消
			if c.ctx.Err() != nil {
				logger.Logrus().Warningln("reconnect cancelled: context cancelled")
				return
			}

			if c.GetState() == StateClosed {
				logger.Logrus().Warningln("reconnect cancelled: client closed")
				return
			}

			logger.Logrus().Warningf("websocket reconnect attempt: %d/%d", attempt, c.maxReconnectAttempts)

			// 确保旧连接完全关闭
			c.stateMux.Lock()
			if c.conn != nil {
				_ = c.conn.CloseNow()
				c.conn = nil
			}
			c.stateMux.Unlock()

			// 等待一段时间再重连，使用指数退避
			if attempt > 1 {
				logger.Logrus().Warningf("waiting %v before reconnect attempt %d", backoffDelay, attempt)
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

			// 重连成功
			c.setState(StateConnected)
			// 重新启动消息处理和心跳
			c.startMessageHandlers()
			c.startHeartbeat()

			if c.onConnected != nil {
				c.onConnected()
			}

			logger.Logrus().Traceln("websocket reconnect successful")
			return
		}

		// 重连失败，关闭客户端
		logger.Logrus().Errorln("websocket reconnect failed after all attempts")
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
		c.stopHeartbeat()

		// 关闭WebSocket连接
		c.stateMux.Lock()
		if c.conn != nil {
			err = c.conn.Close(websocket.StatusNormalClosure, "client closed")
			c.conn = nil
		}
		c.stateMux.Unlock()

		// 取消context，通知所有协程退出
		c.cancel()

		// 等待所有工作协程完成
		done := make(chan struct{})
		go func() {
			c.workerWg.Wait()
			close(done)
		}()

		// 等待所有协程退出，但设置超时防止死锁
		select {
		case <-done:
			logger.Logrus().Traceln("all worker goroutines exited")
		case <-time.After(time.Second * 5):
			logger.Logrus().Warningln("timeout waiting for worker goroutines to exit")
		}

		// 关闭通道
		close(c.receiveChan)
		close(c.sendChan)

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
