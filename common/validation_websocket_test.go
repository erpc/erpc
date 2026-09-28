package common

import (
	"testing"

	"github.com/erpc/erpc/util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestServerConfig_Validate_WebSocket(t *testing.T) {
	defaulted := func() *ServerConfig {
		s := &ServerConfig{ListenV4: util.BoolPtr(false), ListenV6: util.BoolPtr(false)}
		require.NoError(t, s.SetDefaults())
		return s
	}

	t.Run("defaults are valid", func(t *testing.T) {
		require.NoError(t, defaulted().Validate())
	})

	cases := map[string]func(ws *WebSocketServerConfig){
		"pingInterval":                       func(ws *WebSocketServerConfig) { d := Duration(0); ws.PingInterval = &d },
		"sizes":                              func(ws *WebSocketServerConfig) { ws.MaxMessageSize = -1 },
		"maxSubscriptionsPerConnection":      func(ws *WebSocketServerConfig) { ws.MaxSubscriptionsPerConnection = -1 },
		"maxConcurrentRequestsPerConnection": func(ws *WebSocketServerConfig) { ws.MaxConcurrentRequestsPerConnection = -1 },
		"subscriptionBufferSize":             func(ws *WebSocketServerConfig) { ws.SubscriptionBufferSize = -1 },
	}
	for field, mutate := range cases {
		t.Run("invalid "+field+" rejected", func(t *testing.T) {
			s := defaulted()
			mutate(s.WebSocket)
			err := s.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), field)
		})
	}
}
