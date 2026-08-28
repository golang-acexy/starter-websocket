package test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/golang-acexy/starter-websocket/wsstarter"
)

func TestClientSendReceiveAndClose(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		conn, err := websocket.Accept(writer, request, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for {
			messageType, data, readErr := conn.Read(request.Context())
			if readErr != nil {
				return
			}
			if writeErr := conn.Write(request.Context(), messageType, data); writeErr != nil {
				return
			}
		}
	}))
	defer server.Close()

	closed := make(chan struct{})
	client := wsstarter.NewWSClient(context.Background(), wsstarter.WSClientConfig{
		URL:              "ws" + strings.TrimPrefix(server.URL, "http"),
		DisableReconnect: true,
		OnClosed: func(error) {
			close(closed)
		},
	})
	messages, err := client.Connect()
	if err != nil {
		t.Fatalf("connect failed: %v", err)
	}
	if err = client.SendText("hello"); err != nil {
		t.Fatalf("send failed: %v", err)
	}
	select {
	case message := <-messages:
		if message == nil || message.ToString() != "hello" {
			t.Fatalf("unexpected message: %+v", message)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for echoed message")
	}
	if err = client.Ping(); err != nil {
		t.Fatalf("ping failed: %v", err)
	}
	if err = client.Close(); err != nil {
		t.Fatalf("close failed: %v", err)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("close callback was not called")
	}
	if client.GetState() != wsstarter.StateClosed {
		t.Fatalf("unexpected client state: %v", client.GetState())
	}
	if _, open := <-messages; open {
		t.Fatal("receive channel should be closed after workers stop")
	}
}

func TestClientRejectsMissingURL(t *testing.T) {
	client := wsstarter.NewWSClient(context.Background(), wsstarter.WSClientConfig{})
	defer client.Close()
	if _, err := client.Connect(); !errors.Is(err, wsstarter.ErrClientURLMissing) {
		t.Fatalf("expected missing URL error, got: %v", err)
	}
}

func TestClientValidatesReconnectConfiguration(t *testing.T) {
	tests := []struct {
		name     string
		config   wsstarter.WSClientConfig
		expected error
	}{
		{
			name:     "negative attempts",
			config:   wsstarter.WSClientConfig{URL: "ws://unused", MaxReconnectAttempts: -1},
			expected: wsstarter.ErrReconnectAttemptsInvalid,
		},
		{
			name:     "negative interval",
			config:   wsstarter.WSClientConfig{URL: "ws://unused", ReconnectInterval: -time.Second},
			expected: wsstarter.ErrReconnectIntervalInvalid,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := wsstarter.NewWSClient(context.Background(), test.config)
			defer client.Close()
			if _, err := client.Connect(); err != test.expected {
				t.Fatalf("expected %v, got: %v", test.expected, err)
			}
		})
	}
}
