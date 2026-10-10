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

// ConnectionState describes a client lifecycle observation, not a guarantee of transport availability.
type ConnectionState int32

const defaultDataChanLength = 500

const (
	// StateWaitToConnect permits the initial Connect call.
	StateWaitToConnect ConnectionState = iota
	// StateDisconnected indicates an initial dial failure or a disconnection without active retry.
	StateDisconnected
	// StateConnecting indicates an initial dial in progress.
	StateConnecting
	// StateConnected permits queueing and pinging but does not guarantee remote delivery.
	StateConnected
	// StateReconnecting indicates automatic retry after an established connection fails.
	StateReconnecting
	// StateClosed is terminal; create a new client to connect again.
	StateClosed
)

// WSClient owns a transport, message queues, and reconnect workers; create it with NewWSClient.
// Send, state reads, ping, and closure coordinate shared state, but observations can become stale.
// Do not copy after use. Callbacks must handle their own synchronization and failure policy.
type WSClient struct {
	url string

	httpProxyFn func() string
	httpProxy   string

	conn     *websocket.Conn
	response *http.Response
	cancel   context.CancelFunc
	ctx      context.Context
	opts     *websocket.DialOptions

	state   atomic.Int32
	connMux sync.RWMutex

	maxReconnectAttempts int
	reconnectInterval    time.Duration
	disableReconnect     bool
	forceReconnect       bool

	readMaxBytesLimit int64

	blockReceive bool
	receiveChan  chan *Message

	blockSender bool
	sendChan    chan *Message
	sendMux     sync.Mutex

	onConnected    func()
	onDisconnected func(error)
	onError        func(error)
	onClosed       func(error)

	closeOnce   sync.Once
	closeResult error

	workerWg      sync.WaitGroup
	workerMux     sync.Mutex
	workersClosed bool
}

// WSClientConfig defines construction-time transport, queue, retry, and callback policies.
type WSClientConfig struct {
	// URL is the non-empty ws or wss endpoint validated when Connect dials.
	URL string
	// HttpProxyURLFn is consulted per dial and overrides HttpProxyURL; an empty result does not fall back to it.
	HttpProxyURLFn func() string
	// HttpProxyURL supplies a static proxy when HttpProxyURLFn is nil; empty leaves DialOptions unchanged.
	HttpProxyURL string
	// DialOptions supplies library options; headers and subprotocols are copied, but HTTPClient remains shared.
	// A selected proxy replaces HTTPClient.
	DialOptions *websocket.DialOptions

	// DisableReconnect disables automatic reconnect even when ForceReconnect is true.
	DisableReconnect bool
	// ForceReconnect retries normal closures too and uses a practically unbounded attempt limit unless disabled.
	ForceReconnect bool
	// MaxReconnectAttempts bounds automatic retries; zero defaults to three and negatives are rejected by
	// Connect.
	MaxReconnectAttempts int
	// ReconnectInterval defaults to two seconds at zero; negatives are rejected. The first retry is immediate,
	// followed by exponential waits capped after each wait at one minute.
	ReconnectInterval time.Duration

	// ReceiveChanBufferLen sets receive capacity in either blocking mode; nonpositive values default to 500.
	ReceiveChanBufferLen int
	// SendChanBufferLen sets send capacity in either blocking mode; nonpositive values default to 500.
	SendChanBufferLen int
	// ReadMaxBytesLimit overrides the library read limit only when positive; zero and negatives retain its
	// default.
	ReadMaxBytesLimit int64

	// BlockReceive waits for queue capacity; false drops new frames when full. Blocking also delays
	// control-frame processing.
	BlockReceive bool
	// BlockSender waits for queue capacity or cancellation; false returns ErrSendChannelFull when full.
	BlockSender bool

	// OnConnected runs after initial or reconnect success; nil disables it. Keep it short and handle callback
	// failures.
	OnConnected func()
	// OnDisconnected reports a transition away from a connection; callbacks can run in workers, so avoid
	// blocking lifecycle calls.
	OnDisconnected func(error)
	// OnError reports write errors and exhausted retries, not every failure; callbacks from separate paths may
	// overlap.
	OnError func(error)
	// OnClosed runs once after closure waiting; nil disables it. Never call Close or CloseWithError recursively
	// from this callback.
	OnClosed func(error)
}

// NewWSClient copies configuration and starts context monitoring without dialing.
// A nil context uses context.Background; cancellation initiates closure. Use Close
// to release even never-connected or initially failed clients. Header/subprotocol
// options are copied, while callback state and a supplied HTTPClient remain caller-owned.
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
	client.workerWg.Add(1)
	go client.contextMonitor()
	return client
}

// contextMonitor deregisters itself before closing to avoid waiting on its own worker.
func (c *WSClient) contextMonitor() {
	<-c.ctx.Done()
	logger.Logrus().Traceln("context cancelled, initiating graceful shutdown")
	c.workerWg.Done()
	_ = c.Close()
}

// addWorker serializes worker registration against closure.
func (c *WSClient) addWorker() bool {
	c.workerMux.Lock()
	defer c.workerMux.Unlock()
	if c.workersClosed {
		return false
	}
	c.workerWg.Add(1)
	return true
}

func (c *WSClient) setState(state ConnectionState) {
	c.state.Store(int32(state))
}

// GetState atomically observes the current state; it is not a readiness or delivery guarantee.
func (c *WSClient) GetState() ConnectionState {
	return ConnectionState(c.state.Load())
}

func (c *WSClient) transitionState(from, to ConnectionState) bool {
	return c.state.CompareAndSwap(int32(from), int32(to))
}

// IsConnected observes connected state; the transport can disconnect immediately afterward.
func (c *WSClient) IsConnected() bool {
	return c.GetState() == StateConnected
}

// Connect dials once from StateWaitToConnect and returns the receive channel on success.
// Invalid configuration is rejected before the transition. An initial dial failure
// leaves the client disconnected without automatic retry; create a new client to retry.
// Consume messages promptly. Close owns the channel and may leave it open if workers
// do not finish within the closure wait; callers must also observe context and callbacks.
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
	if err := c.dial(); err != nil {
		c.transitionState(StateConnecting, StateDisconnected)
		return nil, fmt.Errorf("failed to connect: %w", err)
	}
	if !c.transitionState(StateConnecting, StateConnected) {
		c.closeCurrentConnection()
		return nil, ErrClientClosing
	}
	c.startMessageHandler()
	c.startMessageSender()
	if c.onConnected != nil {
		c.onConnected()
	}
	logger.Logrus().Traceln("websocket client connected successfully")
	return c.receiveChan, nil
}

// dial applies current proxy selection and publishes a successful transport under the connection lock.
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

// startMessageHandler reads frames until failure or cancellation, applying configured queue backpressure.
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

				messageType, data, err := conn.Read(c.ctx)
				if err != nil {
					if c.ctx.Err() != nil {
						logger.Logrus().Warningln("websocket read interrupted by context cancellation")
						return
					}
					logger.Logrus().Errorf("websocket read error: %v", err)
					c.handleConnectionError(err)
					return
				}
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

// startMessageSender drains the send queue without durable replay across connection failures.
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

				writeCtx, cancel := context.WithTimeout(c.ctx, time.Second*10)
				err := conn.Write(writeCtx, data.Type, data.Data)
				cancel()
				if err != nil {
					if c.ctx.Err() != nil {
						logger.Logrus().Traceln("websocket write interrupted by context cancellation")
						return
					}
					logger.Logrus().Warningf("websocket write error: %v", err)
					if c.onError != nil {
						c.onError(err)
					}
					go c.handleConnectionError(err)
				}
			case <-c.ctx.Done():
				logger.Logrus().Traceln("websocket client sender exit due to context cancellation")
				return
			}
		}
	}()
}

// Send queues a frame while connected; concurrent producers are serialized.
// Success means enqueueing, not remote delivery. Bytes are not copied: hand off
// ownership and do not mutate them while queued or in use. BlockSender waits for
// capacity or cancellation; otherwise a full queue returns ErrSendChannelFull.
// Disconnects and write failures can lose queued frames; message type is not validated here.
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

// SendText allocates and queues text with Send delivery semantics; an empty string is allowed.
func (c *WSClient) SendText(text string) error {
	return c.Send(websocket.MessageText, []byte(text))
}

// SendBinary queues binary data without copying; nil and empty payloads are allowed. Keep queued bytes
// unchanged.
func (c *WSClient) SendBinary(data []byte) error {
	return c.Send(websocket.MessageBinary, data)
}

// handleConnectionError elects one disconnected or reconnecting transition for a transport failure.
func (c *WSClient) handleConnectionError(err error) {
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

// shouldReconnect applies disable-first policy and otherwise filters normal closure unless forced.
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

// reconnect retries transport establishment without restoring application sessions or replaying messages.
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
		maxBackoff := time.Minute
		var maxReconnectAttempts int
		if c.forceReconnect {
			maxReconnectAttempts = math.MaxInt
		} else {
			maxReconnectAttempts = c.maxReconnectAttempts
		}
		for attempt := 1; attempt <= maxReconnectAttempts; attempt++ {
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
				backoffDelay *= 2
				if backoffDelay > maxBackoff {
					backoffDelay = maxBackoff
				}
			}

			if err := c.dial(); err != nil {
				logger.Logrus().Warningf("reconnect attempt %d failed: %v", attempt, err)
				continue
			}
			if !c.transitionState(StateReconnecting, StateConnected) {
				c.closeCurrentConnection()
				return
			}
			logger.Logrus().Debugln("websocket reconnect successful")
			// Restart the reader only; the sender keeps consuming the shared queue.
			c.startMessageHandler()
			if c.onConnected != nil {
				c.onConnected()
			}
			return
		}

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

// Close permanently closes the client using CloseWithError(nil).
// Repeated or concurrent calls share one closure result. Do not invoke it recursively
// from closure callbacks; avoid blocking closure from worker callbacks.
func (c *WSClient) Close() error {
	return c.CloseWithError(nil)
}

// Ping sends a ping and waits for pong with a ten-second deadline; no periodic heartbeat is scheduled.
func (c *WSClient) Ping() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()
	return c.PingContext(ctx)
}

// PingContext sends a ping and waits for pong on the current connected transport.
// A nil context uses context.Background with no deadline. Concurrent calls are
// supported, but a blocked receive worker can delay pong processing. A disconnected
// client returns ErrClientNotConnected; a missing transport returns ErrConnectionNil.
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

// CloseWithError closes once, forwarding closeErr to callbacks rather than returning it.
// The return value is the underlying transport close error, not a worker timeout.
// After transport closure it waits up to five seconds for workers; the receive
// channel closes only if they finish. Queued sends are not flushed. Callbacks execute
// synchronously inside the one-time closure: recursive Close or CloseWithError deadlocks.
// Blocking closure from a worker callback can exhaust the wait and leave receive open.
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

		done := make(chan struct{})
		go func() {
			c.workerWg.Wait()
			logger.Logrus().Traceln("all worker goroutines exited")
			close(done)
		}()

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
		// Never close the receive channel while a worker may still send into it.
		if workersStopped {
			close(c.receiveChan)
		}

		if c.onDisconnected != nil && previousState != StateDisconnected && previousState != StateReconnecting && previousState != StateClosed && previousState != StateWaitToConnect {
			c.onDisconnected(closeErr)
		}

		if c.onClosed != nil {
			c.onClosed(closeErr)
		}
		logger.Logrus().Traceln("websocket client closed successfully")
	})
	return c.closeResult
}
