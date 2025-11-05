# WebSocket Subscription Design Document

**Version:** 1.0  
**Date:** 2025-01-XX  
**Status:** Draft

---

## Table of Contents

1. [Overview](#1-overview)
2. [Architecture](#2-architecture)
3. [Package Structure](#3-package-structure)
4. [Component Specifications](#4-component-specifications)
5. [Configuration](#5-configuration)
6. [Data Flow](#6-data-flow)
7. [Error Handling](#7-error-handling)
8. [Lifecycle Management](#8-lifecycle-management)
9. [Metrics & Observability](#9-metrics--observability)
10. [Testing Strategy](#10-testing-strategy)
11. [Implementation Phases](#11-implementation-phases)
12. [Design Decisions](#12-design-decisions)
13. [Security Considerations](#13-security-considerations)
14. [References](#14-references)

---

## 1. Overview

### 1.1 Goals

Add WebSocket support to eRPC to enable real-time blockchain event subscriptions (`eth_subscribe`/`eth_unsubscribe`) for clients.

**Key Requirements:**
- WebSocket connections on **server-side only**
- HTTP(S) connections to upstream providers (no WS to upstreams)
- Support for `eth_subscribe("newHeads")` and `eth_subscribe("logs", filter)`
- Same HTTP port for both HTTP and WebSocket connections
- One network per WebSocket connection (simplified scope)
- Decoupled design from existing implementation

### 1.2 Design Principles

1. **Decoupling:** WebSocket and Subscription packages are independent from core eRPC
2. **Interface-based:** Use interfaces to connect to existing Network/Upstream
3. **Polling-based:** Poll upstreams via HTTP to gather data for subscriptions
4. **Graceful degradation:** Failover and error handling via existing infrastructure
5. **Observable:** Comprehensive metrics and logging

### 1.3 Non-Goals

- WebSocket connections to upstream providers
- Multiple networks per WebSocket connection
- Support for subscriptions other than `newHeads` and `logs`
- Real-time (sub-second) latency guarantees

---

## 2. Architecture

### 2.1 High-Level Component Diagram

```
┌────────────────────────────────────────────────────────────────┐
│                         eRPC Server                            │
│                                                                │
│  ┌──────────────┐         ┌─────────────────────────────┐    │
│  │              │         │    WebSocket Package        │    │
│  │   HTTP       │         │  /erpc/websocket/          │    │
│  │   Server     │────────▶│                             │    │
│  │              │ upgrade │  ┌──────────────────────┐   │    │
│  └──────────────┘         │  │  Server              │   │    │
│                           │  │  (upgrade handler)   │   │    │
│                           │  └──────────────────────┘   │    │
│                           │           │                 │    │
│                           │           ▼                 │    │
│                           │  ┌──────────────────────┐   │    │
│                           │  │  ConnectionManager   │   │    │
│                           │  │  (per network)       │   │    │
│                           │  └──────────────────────┘   │    │
│                           │           │                 │    │
│                           │           ▼                 │    │
│                           │  ┌──────────────────────┐   │    │
│                           │  │  Connection          │   │    │
│                           │  │  (per client)        │   │    │
│                           │  └──────────────────────┘   │    │
│                           └─────────────────────────────┘    │
│                                      │                        │
│                                      ▼                        │
│  ┌─────────────────────────────────────────────────────┐    │
│  │       Subscription Package                           │    │
│  │       /erpc/subscription/                            │    │
│  │                                                       │    │
│  │  ┌──────────────┐    ┌──────────────┐              │    │
│  │  │ Manager      │    │  Pollers     │              │    │
│  │  │ (subscribe/  │    │  (fetch data)│              │    │
│  │  │  unsubscribe)│    │              │              │    │
│  │  └──────────────┘    └──────────────┘              │    │
│  └─────────────────────────────────────────────────────┘    │
│                                      │                        │
│                                      ▼                        │
│  ┌─────────────────────────────────────────────────────┐    │
│  │          Existing Network/Upstream                   │    │
│  │          (HTTP requests to upstreams)                │    │
│  └─────────────────────────────────────────────────────┘    │
└────────────────────────────────────────────────────────────────┘
```

### 2.2 Component Responsibilities

| Component | Responsibility |
|-----------|----------------|
| **WebSocket Server** | Handle HTTP→WS upgrades, manage ConnectionManagers |
| **ConnectionManager** | Manage all connections for a specific network |
| **Connection** | Handle individual client WebSocket connection |
| **Subscription Manager** | Handle subscribe/unsubscribe lifecycle |
| **Registry** | Track all active subscriptions |
| **Pollers** | Fetch data from upstreams via HTTP |
| **Broadcaster** | Send notifications to subscribed connections |

---

## 3. Package Structure

```
erpc/
├── websocket/                      # NEW: WebSocket transport layer
│   ├── server.go                   # WS upgrade handler
│   ├── connection.go               # Individual WS connection
│   ├── connection_manager.go      # Manages connections per network
│   ├── message.go                  # JSON-RPC message types
│   └── config.go                   # WS-specific config
│
├── subscription/                   # NEW: Subscription business logic
│   ├── manager.go                  # Subscription lifecycle
│   ├── registry.go                 # Maps subId → subscribers
│   ├── types.go                    # Subscription types & filters
│   ├── poller_head.go             # NewHeads poller
│   ├── poller_logs.go             # Logs poller
│   ├── poller_interface.go        # Common poller interface
│   └── broadcaster.go              # Broadcast notifications
│
├── erpc.go                         # Existing (minimal changes)
├── http_server.go                  # Existing (add WS upgrade)
├── networks.go                     # Existing (no changes)
└── projects.go                     # Existing (no changes)
```

---

## 4. Component Specifications

### 4.1 WebSocket Package (`/erpc/websocket/`)

#### 4.1.1 Decoupling Interfaces

```go
// ForwardFunc is a function that forwards JSON-RPC requests
// This decouples websocket package from Network implementation
type ForwardFunc func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error)

// NetworkInfo provides minimal network information
type NetworkInfo interface {
    Id() string
    ProjectId() string
    Architecture() common.NetworkArchitecture
}
```

#### 4.1.2 Server (`server.go`)

**Purpose:** Handle WebSocket upgrades and manage ConnectionManagers.

```go
type Server struct {
    upgrader       websocket.Upgrader
    connManagers   sync.Map  // networkId → *ConnectionManager
    logger         *zerolog.Logger
}

// Key Methods:
func (s *Server) Upgrade(w http.ResponseWriter, r *http.Request, 
    networkInfo NetworkInfo, forwardFunc ForwardFunc) error

func (s *Server) GetOrCreateManager(networkInfo NetworkInfo, 
    forwardFunc ForwardFunc) *ConnectionManager
```

**Responsibilities:**
- Upgrade HTTP connections to WebSocket
- Create/retrieve ConnectionManager per network
- Validate upgrade requests

#### 4.1.3 ConnectionManager (`connection_manager.go`)

**Purpose:** Manage all WebSocket connections for a specific network.

```go
type ConnectionManager struct {
    networkInfo   NetworkInfo
    forwardFunc   ForwardFunc
    
    connections   sync.Map  // *Connection → bool
    subManager    *subscription.Manager
    
    logger        *zerolog.Logger
    ctx           context.Context
    cancel        context.CancelFunc
}

// Key Methods:
func NewConnectionManager(ctx context.Context, networkInfo NetworkInfo, 
    forwardFunc ForwardFunc, logger *zerolog.Logger) *ConnectionManager

func (cm *ConnectionManager) AddConnection(conn *Connection)
func (cm *ConnectionManager) RemoveConnection(conn *Connection)
func (cm *ConnectionManager) Shutdown()
```

**Responsibilities:**
- Track all active connections for a network
- Create and manage Subscription Manager
- Handle graceful shutdown

#### 4.1.4 Connection (`connection.go`)

**Purpose:** Represent a single WebSocket client connection.

```go
type Connection struct {
    id            string
    conn          *websocket.Conn
    manager       *ConnectionManager
    
    subscriptions sync.Map  // subId → *subscription.Subscription
    
    send          chan []byte
    done          chan struct{}
    
    logger        *zerolog.Logger
    mu            sync.RWMutex
}

// Key Methods:
func (c *Connection) Start()
func (c *Connection) HandleMessage(msg []byte) error
func (c *Connection) SendNotification(subId string, data interface{}) error
func (c *Connection) Close()

// Internal goroutines:
func (c *Connection) reader()  // Read from client
func (c *Connection) writer()  // Write to client
```

**Responsibilities:**
- Handle client message reading/writing
- Process JSON-RPC requests (`eth_subscribe`, `eth_unsubscribe`)
- Send subscription notifications
- Track connection-specific subscriptions

#### 4.1.5 Message Types (`message.go`)

```go
type JsonRpcRequest struct {
    Jsonrpc string          `json:"jsonrpc"`
    Id      interface{}     `json:"id,omitempty"`
    Method  string          `json:"method"`
    Params  json.RawMessage `json:"params,omitempty"`
}

type JsonRpcResponse struct {
    Jsonrpc string      `json:"jsonrpc"`
    Id      interface{} `json:"id"`
    Result  interface{} `json:"result,omitempty"`
    Error   *RpcError   `json:"error,omitempty"`
}

type JsonRpcNotification struct {
    Jsonrpc string                 `json:"jsonrpc"`
    Method  string                 `json:"method"`
    Params  *SubscriptionNotification `json:"params"`
}

type SubscriptionNotification struct {
    Subscription string      `json:"subscription"`
    Result       interface{} `json:"result"`
}

type RpcError struct {
    Code    int         `json:"code"`
    Message string      `json:"message"`
    Data    interface{} `json:"data,omitempty"`
}
```

---

### 4.2 Subscription Package (`/erpc/subscription/`)

#### 4.2.1 Manager (`manager.go`)

**Purpose:** Handle subscription lifecycle.

```go
type Manager struct {
    networkInfo   websocket.NetworkInfo
    forwardFunc   websocket.ForwardFunc
    
    registry      *Registry
    broadcaster   *Broadcaster
    
    headPoller    Poller
    logsPoller    Poller
    
    logger        *zerolog.Logger
    ctx           context.Context
}

// Key Methods:
func NewManager(ctx context.Context, networkInfo websocket.NetworkInfo, 
    forwardFunc websocket.ForwardFunc, logger *zerolog.Logger) *Manager

func (m *Manager) Subscribe(connId string, subType SubscriptionType, 
    params interface{}) (string, error)

func (m *Manager) Unsubscribe(connId string, subId string) error
func (m *Manager) RegisterCallback(connId string, callback NotificationCallback)
func (m *Manager) UnregisterCallback(connId string)
func (m *Manager) Start()
func (m *Manager) Shutdown()
```

**Responsibilities:**
- Create/destroy subscriptions
- Start/stop pollers
- Coordinate between Registry and Broadcaster
- Manage callbacks for connections

#### 4.2.2 Registry (`registry.go`)

**Purpose:** Track all active subscriptions.

```go
type Registry struct {
    subscriptions sync.Map  // subId → *Subscription
    connSubs      sync.Map  // connId → map[subId]*Subscription
    
    mu sync.RWMutex
}

// Key Methods:
func (r *Registry) Add(sub *Subscription) error
func (r *Registry) Remove(subId string)
func (r *Registry) GetByConnection(connId string) []*Subscription
func (r *Registry) GetByType(subType SubscriptionType) []*Subscription
func (r *Registry) RemoveByConnection(connId string)
```

**Responsibilities:**
- Store and index subscriptions
- Provide fast lookups by connection, type, or ID
- Clean up on connection close

#### 4.2.3 Types (`types.go`)

```go
type SubscriptionType string

const (
    SubTypeNewHeads SubscriptionType = "newHeads"
    SubTypeLogs     SubscriptionType = "logs"
)

type Subscription struct {
    ID         string
    Type       SubscriptionType
    ConnId     string
    Params     interface{}  // LogFilter for logs, nil for newHeads
    CreatedAt  time.Time
}

type LogFilter struct {
    Address   []string      `json:"address,omitempty"`
    Topics    [][]string    `json:"topics,omitempty"`
    FromBlock *string       `json:"fromBlock,omitempty"`
    ToBlock   *string       `json:"toBlock,omitempty"`
}

type NotificationCallback func(subId string, data interface{}) error
```

#### 4.2.4 Poller Interface (`poller_interface.go`)

```go
type Poller interface {
    Start(ctx context.Context)
    Stop()
    Name() string
}

type PollConfig struct {
    Interval      time.Duration
    MaxRetries    int
    RetryDelay    time.Duration
}
```

#### 4.2.5 HeadPoller (`poller_head.go`)

**Purpose:** Poll for new blocks.

```go
type HeadPoller struct {
    networkInfo  websocket.NetworkInfo
    forwardFunc  websocket.ForwardFunc
    broadcaster  *Broadcaster
    registry     *Registry
    
    config       *PollConfig
    lastBlock    *Block
    
    logger       *zerolog.Logger
    mu           sync.RWMutex
}

// Key Methods:
func NewHeadPoller(...) *HeadPoller
func (p *HeadPoller) Start(ctx context.Context)
func (p *HeadPoller) poll(ctx context.Context)
func (p *HeadPoller) fetchLatestBlock(ctx context.Context) (*Block, error)

type Block struct {
    Number       string   `json:"number"`
    Hash         string   `json:"hash"`
    ParentHash   string   `json:"parentHash"`
    Timestamp    string   `json:"timestamp"`
    // ... additional fields as needed
}
```

**Polling Logic:**
1. Fetch latest block via `eth_blockNumber` + `eth_getBlockByNumber`
2. Compare with last seen block number
3. If new block detected, broadcast to all `newHeads` subscribers
4. Update `lastBlock`

#### 4.2.6 LogsPoller (`poller_logs.go`)

**Purpose:** Poll for new logs matching active filters.

```go
type LogsPoller struct {
    networkInfo  websocket.NetworkInfo
    forwardFunc  websocket.ForwardFunc
    broadcaster  *Broadcaster
    registry     *Registry
    
    config       *PollConfig
    lastPolled   map[string]int64  // filterId → lastBlockNumber
    
    logger       *zerolog.Logger
    mu           sync.RWMutex
}

// Key Methods:
func NewLogsPoller(...) *LogsPoller
func (p *LogsPoller) Start(ctx context.Context)
func (p *LogsPoller) poll(ctx context.Context)
func (p *LogsPoller) fetchLogs(ctx context.Context, filter *LogFilter, 
    fromBlock, toBlock int64) ([]*Log, error)

type Log struct {
    Address          string   `json:"address"`
    Topics           []string `json:"topics"`
    Data             string   `json:"data"`
    BlockNumber      string   `json:"blockNumber"`
    TransactionHash  string   `json:"transactionHash"`
    // ... additional fields
}
```

**Polling Logic:**
1. Get all active log subscriptions from Registry
2. For each unique filter, fetch logs from `lastPolled[filterId]` to current block
3. Match logs to subscriptions
4. Broadcast matching logs
5. Update `lastPolled[filterId]`

#### 4.2.7 Broadcaster (`broadcaster.go`)

**Purpose:** Send notifications to connections.

```go
type Broadcaster struct {
    callbacks sync.Map  // connId → NotificationCallback
    logger    *zerolog.Logger
}

// Key Methods:
func NewBroadcaster(logger *zerolog.Logger) *Broadcaster
func (b *Broadcaster) RegisterCallback(connId string, callback NotificationCallback)
func (b *Broadcaster) UnregisterCallback(connId string)
func (b *Broadcaster) BroadcastNewHead(subscriptions []*Subscription, block *Block)
func (b *Broadcaster) BroadcastLogs(subscriptions []*Subscription, logs []*Log)
func (b *Broadcaster) notify(connId, subId string, data interface{})
```

**Broadcasting Logic:**
- Iterate through relevant subscriptions
- Call registered callback for each connection
- Handle callback errors gracefully (log and continue)

---

### 4.3 Integration with Existing Code

#### 4.3.1 HttpServer Changes (`http_server.go`)

**Minimal modifications required:**

```go
// 1. Add field to HttpServer
type HttpServer struct {
    // ... existing fields
    wsServer  *websocket.Server  // NEW
}

// 2. Initialize in NewHttpServer
func NewHttpServer(...) (*HttpServer, error) {
    // ... existing code
    srv.wsServer = websocket.NewServer(logger)
    // ... rest
}

// 3. Add WebSocket upgrade check in request handler
func (s *HttpServer) createRequestHandler() http.Handler {
    handleRequest := func(httpCtx context.Context, r *http.Request, 
        w http.ResponseWriter, writeFatalError func(...)) {
        
        // Check for WebSocket upgrade FIRST
        if websocket.IsWebSocketUpgrade(r) {
            s.handleWebSocketUpgrade(w, r, projectId, architecture, chainId)
            return
        }
        
        // ... existing HTTP handling code
    }
}

// 4. New method: Handle WebSocket upgrade
func (s *HttpServer) handleWebSocketUpgrade(
    w http.ResponseWriter,
    r *http.Request,
    projectId, architecture, chainId string,
) {
    // Get network
    networkId := fmt.Sprintf("%s:%s", architecture, chainId)
    network, err := s.erpc.GetNetwork(r.Context(), projectId, networkId)
    if err != nil {
        http.Error(w, err.Error(), http.StatusNotFound)
        return
    }
    
    // Create forward function (decouples WS from Network)
    forwardFunc := func(ctx context.Context, req *common.NormalizedRequest) (*common.NormalizedResponse, error) {
        return network.Forward(ctx, req)
    }
    
    // Upgrade to WebSocket
    err = s.wsServer.Upgrade(w, r, network, forwardFunc)
    if err != nil {
        s.logger.Error().Err(err).Msg("failed to upgrade to websocket")
        return
    }
}
```

**Key Points:**
- ✅ Same HTTP port for WebSocket
- ✅ Reuse existing auth/CORS/routing
- ✅ Network isolation preserved
- ✅ Minimal changes to existing code

---

## 5. Configuration

### 5.1 Configuration Schema

#### Add to `ServerConfig`:

```go
type ServerConfig struct {
    // ... existing fields
    
    WebSocket *WebSocketConfig `yaml:"websocket,omitempty" json:"websocket,omitempty"`
}

type WebSocketConfig struct {
    Enabled                       *bool    `yaml:"enabled,omitempty" json:"enabled,omitempty"`
    MaxConnectionsPerNetwork      int      `yaml:"maxConnectionsPerNetwork,omitempty" json:"maxConnectionsPerNetwork,omitempty"`
    MaxSubscriptionsPerConnection int      `yaml:"maxSubscriptionsPerConnection,omitempty" json:"maxSubscriptionsPerConnection,omitempty"`
    PingInterval                  Duration `yaml:"pingInterval,omitempty" json:"pingInterval,omitempty"`
    PongTimeout                   Duration `yaml:"pongTimeout,omitempty" json:"pongTimeout,omitempty"`
    ReadBufferSize                int      `yaml:"readBufferSize,omitempty" json:"readBufferSize,omitempty"`
    WriteBufferSize               int      `yaml:"writeBufferSize,omitempty" json:"writeBufferSize,omitempty"`
}
```

#### Add to `NetworkConfig`:

```go
type NetworkConfig struct {
    // ... existing fields
    
    Subscription *SubscriptionConfig `yaml:"subscription,omitempty" json:"subscription,omitempty"`
}

type SubscriptionConfig struct {
    PollInterval  Duration `yaml:"pollInterval,omitempty" json:"pollInterval,omitempty"`
    MaxLogFilters int      `yaml:"maxLogFilters,omitempty" json:"maxLogFilters,omitempty"`
}
```

### 5.2 Default Values

```go
// WebSocket defaults
enabled: true
maxConnectionsPerNetwork: 10000
maxSubscriptionsPerConnection: 100
pingInterval: 30s
pongTimeout: 60s
readBufferSize: 4096
writeBufferSize: 4096

// Subscription defaults
pollInterval: 2s  // Will be dynamic in Phase 4
maxLogFilters: 50
```

### 5.3 Example Configuration

```yaml
logLevel: info

server:
  httpPort: 4000
  websocket:
    enabled: true
    maxConnectionsPerNetwork: 10000
    maxSubscriptionsPerConnection: 100
    pingInterval: 30s
    pongTimeout: 60s

projects:
  - id: main
    networks:
      - architecture: evm
        evm:
          chainId: 1
        subscription:
          pollInterval: 2s
          maxLogFilters: 50
      
      - architecture: evm
        evm:
          chainId: 42161  # Arbitrum - faster blocks
        subscription:
          pollInterval: 500ms  # More frequent polling
          maxLogFilters: 100
```

---

## 6. Data Flow

### 6.1 Connection Establishment

```
Client              HttpServer         WsServer        ConnectionManager
  |                     |                  |                   |
  |--HTTP Upgrade------>|                  |                   |
  |                     |--Upgrade-------->|                   |
  |                     |                  |--GetOrCreate----->|
  |                     |                  |<-Manager----------|
  |                     |                  |--NewConnection--->|
  |<--101 Switching-----|<-----------------|                   |
  |                     |                  |                   |
  |                     |                  |<-Conn Added-------|
  |                     |                  |                   |
```

### 6.2 eth_subscribe Flow

```
Client         Connection       Manager        Registry       Poller
  |                |               |               |             |
  |--subscribe---->|               |               |             |
  |                |--Subscribe--->|               |             |
  |                |               |--Add--------->|             |
  |                |               |<-OK-----------|             |
  |                |               |               |             |
  |                |               |--StartIfNeeded------------->|
  |                |               |               |             |
  |<--subId--------|<-subId--------|               |             |
  |                |               |               |             |
```

### 6.3 Notification Flow

```
Poller        Broadcaster     Connection      Client
  |                |               |             |
  |--poll--------->|               |             |
  |<-new data------|               |             |
  |                |               |             |
  |--Broadcast---->|               |             |
  |                |--Notify------>|             |
  |                |               |--JSON-RPC-->|
  |                |               |             |
```

### 6.4 eth_unsubscribe Flow

```
Client         Connection       Manager        Registry
  |                |               |               |
  |--unsubscribe-->|               |               |
  |                |--Unsubscribe->|               |
  |                |               |--Remove------>|
  |                |               |<-OK-----------|
  |<--success------|<-success------|               |
  |                |               |               |
```

### 6.5 Connection Close Flow

```
Client/Server  Connection     Manager        Registry
  |                |             |               |
  |--close-------->|             |               |
  |                |--Cleanup--->|               |
  |                |             |--RemoveAll--->|
  |                |             |<-OK-----------|
  |                |--Callback-->|               |
  |                |             |               |
  |                |--Close------|               |
  |                |             |               |
```

---

## 7. Error Handling

### 7.1 Connection Errors

| Error Type | Handling Strategy |
|------------|-------------------|
| **Read errors** | Log error, close connection gracefully |
| **Write errors** | Buffer briefly (5s), then close if persistent |
| **Parse errors** | Send JSON-RPC error response (-32700) |
| **Timeout** | Close connection after pongTimeout |

### 7.2 Subscription Errors

| Error | Code | Message | Action |
|-------|------|---------|--------|
| **Invalid params** | -32602 | "Invalid params" | Return error response |
| **Too many subscriptions** | -32005 | "Subscription limit exceeded" | Return error response |
| **Unknown subscription** | -32001 | "Subscription not found" | Return error on unsubscribe |
| **Invalid filter** | -32602 | "Invalid log filter" | Return error response |

### 7.3 Polling Errors

| Error Type | Handling Strategy |
|------------|-------------------|
| **Upstream failures** | Use existing failsafe/retry logic |
| **Parse errors** | Log error, skip notification, continue |
| **Broadcast errors** | Log per-connection error, continue to others |
| **Network timeout** | Retry with backoff |

### 7.4 Error Response Format

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "error": {
    "code": -32602,
    "message": "Invalid params",
    "data": "Address must be a valid hex string"
  }
}
```

---

## 8. Lifecycle Management

### 8.1 Startup Sequence

```
1. eRPC starts
2. HttpServer initializes
3. WebSocket.Server created
4. First WS connection arrives for network X
5. ConnectionManager created for network X
6. Subscription.Manager created
7. Connection established
8. First subscription created
9. Pollers started (lazy initialization)
```

### 8.2 Shutdown Sequence

```
1. Shutdown signal received (SIGINT/SIGTERM)
2. Context cancelled
3. HttpServer stops accepting new connections
4. For each ConnectionManager:
   a. Stop all pollers
   b. Send WebSocket close frame (1001) to all connections
   c. Wait for connections to close (5s timeout)
   d. Force close remaining connections
5. Cleanup resources
```

### 8.3 Connection Lifecycle

```
┌─────────────┐
│   Created   │
└──────┬──────┘
       │
       ▼
┌─────────────┐
│   Active    │◄──────┐
└──────┬──────┘       │
       │              │
       │ (subscriptions)
       │              │
       │              │
       ▼              │
┌─────────────┐       │
│  Subscribed │───────┘
└──────┬──────┘
       │
       ▼
┌─────────────┐
│   Closing   │
└──────┬──────┘
       │
       ▼
┌─────────────┐
│   Closed    │
└─────────────┘
```

---

## 9. Metrics & Observability

### 9.1 Prometheus Metrics

```go
// Connection metrics
websocket_connections_active{project, network}
  - Gauge: Current active connections

websocket_connections_total{project, network}
  - Counter: Total connections established

websocket_connections_closed_total{project, network, reason}
  - Counter: Total connections closed (by reason)

// Subscription metrics
websocket_subscriptions_active{project, network, type}
  - Gauge: Current active subscriptions

websocket_subscriptions_total{project, network, type}
  - Counter: Total subscriptions created

websocket_subscriptions_errors_total{project, network, type, error_code}
  - Counter: Subscription errors

// Polling metrics
websocket_poll_duration_seconds{network, type}
  - Histogram: Time to complete poll

websocket_poll_errors_total{network, type, error}
  - Counter: Polling errors

websocket_poll_items_total{network, type}
  - Counter: Items fetched (blocks, logs)

// Message metrics
websocket_messages_received_total{project, network, method}
  - Counter: Messages received from clients

websocket_messages_sent_total{project, network, type}
  - Counter: Messages sent to clients (responses, notifications)

websocket_message_errors_total{project, network, error_type}
  - Counter: Message processing errors

// Notification metrics
websocket_notifications_sent_total{network, subscription_type}
  - Counter: Notifications sent

websocket_notifications_dropped_total{network, subscription_type, reason}
  - Counter: Notifications that couldn't be delivered
```

### 9.2 Logging Standards

#### Connection Events
```go
// Connection established
logger.Info().
    Str("connId", id).
    Str("remoteAddr", addr).
    Str("networkId", networkId).
    Msg("websocket connection established")

// Connection closed
logger.Info().
    Str("connId", id).
    Str("reason", reason).
    Dur("duration", duration).
    Msg("websocket connection closed")
```

#### Subscription Events
```go
// Subscription created
logger.Debug().
    Str("subId", id).
    Str("type", string(type)).
    Str("connId", connId).
    Interface("params", params).
    Msg("subscription created")

// Subscription removed
logger.Debug().
    Str("subId", id).
    Str("connId", connId).
    Msg("subscription removed")
```

#### Polling Events
```go
// New block detected
logger.Trace().
    Int64("blockNumber", num).
    Str("blockHash", hash).
    Int("subscribers", count).
    Msg("new block detected")

// New logs detected
logger.Trace().
    Int("count", len(logs)).
    Int64("fromBlock", from).
    Int64("toBlock", to).
    Msg("new logs detected")

// Polling error
logger.Warn().
    Err(err).
    Str("poller", pollerName).
    Int("attempt", attempt).
    Msg("polling error")
```

### 9.3 Tracing

Integrate with existing OpenTelemetry tracing:

```go
// Span for subscription creation
ctx, span := common.StartSpan(ctx, "websocket.Subscribe",
    trace.WithAttributes(
        attribute.String("network.id", networkId),
        attribute.String("subscription.type", string(subType)),
    ),
)
defer span.End()

// Span for polling
ctx, span := common.StartSpan(ctx, "websocket.Poll",
    trace.WithAttributes(
        attribute.String("network.id", networkId),
        attribute.String("poller.type", pollerType),
    ),
)
defer span.End()
```

---

## 10. Testing Strategy

### 10.1 Unit Tests

#### WebSocket Package Tests

```go
// websocket/connection_test.go
- TestConnection_HandleMessage
- TestConnection_SendNotification
- TestConnection_Close
- TestConnection_ReadTimeout
- TestConnection_WriteTimeout

// websocket/connection_manager_test.go
- TestConnectionManager_AddConnection
- TestConnectionManager_RemoveConnection
- TestConnectionManager_Shutdown
- TestConnectionManager_MaxConnections
```

#### Subscription Package Tests

```go
// subscription/manager_test.go
- TestManager_Subscribe
- TestManager_Unsubscribe
- TestManager_SubscribeTwice
- TestManager_MultipleSubscriptions
- TestManager_SubscriptionLimits

// subscription/registry_test.go
- TestRegistry_Add
- TestRegistry_Remove
- TestRegistry_GetByConnection
- TestRegistry_GetByType
- TestRegistry_RemoveByConnection

// subscription/poller_head_test.go
- TestHeadPoller_FetchBlock
- TestHeadPoller_DetectNewBlock
- TestHeadPoller_MissedBlocks
- TestHeadPoller_ErrorRecovery
- TestHeadPoller_Reorg

// subscription/poller_logs_test.go
- TestLogsPoller_FetchLogs
- TestLogsPoller_FilterMatching
- TestLogsPoller_Deduplication
- TestLogsPoller_ErrorRecovery

// subscription/broadcaster_test.go
- TestBroadcaster_Broadcast
- TestBroadcaster_MultipleSubscribers
- TestBroadcaster_FailedCallback
```

### 10.2 Integration Tests

```go
// websocket_integration_test.go
- TestWebSocket_SubscribeNewHeads
  * Connect, subscribe, receive notifications
  
- TestWebSocket_SubscribeLogs
  * Connect, subscribe with filter, receive matching logs
  
- TestWebSocket_Unsubscribe
  * Subscribe, unsubscribe, verify no notifications
  
- TestWebSocket_MultipleSubscriptions
  * Create multiple subscriptions, verify all work
  
- TestWebSocket_Reconnect
  * Disconnect, reconnect, resubscribe
  
- TestWebSocket_MultipleClients
  * Multiple clients, same subscriptions, all receive
  
- TestWebSocket_InvalidParams
  * Send invalid subscribe params, get error
  
- TestWebSocket_MaxSubscriptions
  * Exceed max subscriptions, get error
```

### 10.3 E2E Tests

#### JavaScript Client Tests

```javascript
// test/websocket_e2e_test.js
const WebSocket = require('ws');

describe('WebSocket E2E', () => {
  test('subscribe to newHeads', async () => {
    const ws = new WebSocket('ws://localhost:4000/main/evm/1');
    
    const response = await sendRequest(ws, {
      jsonrpc: '2.0',
      id: 1,
      method: 'eth_subscribe',
      params: ['newHeads']
    });
    
    expect(response.result).toMatch(/^0x[0-9a-f]{32}$/);
    
    const notification = await waitForNotification(ws);
    expect(notification.params.subscription).toBe(response.result);
    expect(notification.params.result).toHaveProperty('number');
  });
  
  test('subscribe to logs', async () => {
    const ws = new WebSocket('ws://localhost:4000/main/evm/1');
    
    const filter = {
      address: '0x...',
      topics: [['0x...']
    };
    
    const response = await sendRequest(ws, {
      jsonrpc: '2.0',
      id: 1,
      method: 'eth_subscribe',
      params: ['logs', filter]
    });
    
    const notification = await waitForNotification(ws, 30000);
    expect(notification.params.result).toHaveProperty('address');
  });
});
```

### 10.4 Load Tests

```go
// Load test scenario
- 1000 concurrent connections
- Each with 10 subscriptions (5 newHeads, 5 logs)
- Run for 10 minutes
- Measure:
  * Connection establishment time
  * Subscription creation time
  * Notification latency (block time to delivery)
  * Memory usage
  * CPU usage
  * Error rate
```

---

## 11. Implementation Phases

### Phase 1: Foundation (Week 1)

**Goal:** Basic WebSocket infrastructure

- [ ] Create package structure
  - [ ] `erpc/websocket/` package
  - [ ] `erpc/subscription/` package
- [ ] Implement `websocket.Server`
- [ ] Implement `websocket.Connection`
- [ ] Implement `websocket.ConnectionManager`
- [ ] Implement `websocket.Message` types
- [ ] HTTP upgrade integration in `http_server.go`
- [ ] Basic connection lifecycle (connect, ping/pong, close)
- [ ] Unit tests for connection handling

**Deliverable:** Clients can connect via WebSocket and receive ping/pong

### Phase 2: NewHeads Subscription (Week 2)

**Goal:** Working `eth_subscribe("newHeads")`

- [ ] Implement `subscription.Manager`
- [ ] Implement `subscription.Registry`
- [ ] Implement `subscription.Types`
- [ ] Implement `subscription.HeadPoller`
- [ ] Implement `subscription.Broadcaster`
- [ ] Handle `eth_subscribe` / `eth_unsubscribe` requests
- [ ] End-to-end newHeads flow
- [ ] Error handling for subscriptions
- [ ] Integration tests

**Deliverable:** Clients can subscribe to newHeads and receive notifications

### Phase 3: Logs Subscription (Week 3)

**Goal:** Working `eth_subscribe("logs", filter)`

- [ ] Implement `subscription.LogsPoller`
- [ ] Filter parsing and validation
- [ ] Filter matching logic
- [ ] Log deduplication
- [ ] Handle complex filters (multiple addresses, topics)
- [ ] Integration tests for logs
- [ ] Performance optimization

**Deliverable:** Clients can subscribe to logs with filters

### Phase 4: Polish & Optimization (Week 4)

**Goal:** Production-ready

- [ ] Configuration validation
- [ ] Comprehensive error handling
- [ ] Metrics implementation
- [ ] Graceful shutdown
- [ ] Connection limits enforcement
- [ ] Rate limiting integration
- [ ] Documentation
- [ ] Load testing
- [ ] Dynamic poll interval (based on blocktime detection)
- [ ] Memory profiling and optimization

**Deliverable:** Production-ready WebSocket implementation

---

## 12. Design Decisions

### 12.1 HTTP POST eth_subscribe Behavior

**Question:** What should happen if client sends `eth_subscribe` via HTTP POST?

**Decision:** Return JSON-RPC error

```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "error": {
    "code": -32000,
    "message": "eth_subscribe is only available via WebSocket connection. Please upgrade to WebSocket."
  }
}
```

**Rationale:**
- Clear error message guides users
- Standard JSON-RPC error format
- Doesn't silently fail

### 12.2 Connection Limits Enforcement

**Question:** How to enforce connection limits?

**Decision:**
- Per-network connection limit (configurable, default: 10000)
- Reject new connections with WebSocket close code 1008 (Policy Violation)
- Log rejected connections with metrics

**Rationale:**
- Prevents resource exhaustion
- Standard WebSocket close code
- Observable via metrics

### 12.3 Subscription ID Format

**Question:** What format for subscription IDs?

**Decision:** Use `0x` + 32 hex characters (same as Geth/Infura)

```go
func generateSubscriptionId() string {
    randomBytes := make([]byte, 16)
    rand.Read(randomBytes)
    return "0x" + hex.EncodeToString(randomBytes)
}
```

**Rationale:**
- Compatible with existing clients
- Sufficient entropy (128 bits)
- Easy to validate

### 12.4 Block Caching Strategy

**Question:** Should pollers cache recent blocks?

**Decision:** Yes, keep last 10 blocks in memory (ring buffer)

**Rationale:**
- Prevents duplicate `eth_getBlockByNumber` calls
- Useful for log polling (needs block timestamps)
- Small memory footprint (~10KB per block)
- Improves performance

### 12.5 Polling Strategy

**Question:** Single poller vs per-subscription poller?

**Decision:** Single poller per subscription type per network

**Rationale:**
- Reduces upstream request volume
- Easier to implement
- Acceptable latency (1-2s)
- Can be optimized later if needed

### 12.6 Dynamic Poll Interval

**Question:** When to implement dynamic polling based on blocktime?

**Decision:** Phase 4 (after MVP is stable)

**Implementation approach:**
```go
// Measure blocktime on startup or periodically
blockTimes := measureBlockTimes(last100Blocks)
avgBlockTime := average(blockTimes)
pollInterval := avgBlockTime * 0.5  // Poll at 50% of avg blocktime

// Update poll interval dynamically
if abs(currentAvg - avgBlockTime) > threshold {
    poller.UpdateInterval(avgBlockTime * 0.5)
}
```

**Rationale:**
- Fixed interval works for MVP
- Dynamic adjustment adds complexity
- Different chains have different blocktimes
- Can optimize after gathering real-world usage data

### 12.7 Filter Complexity Limits

**Question:** Should we limit log filter complexity?

**Decision:** Yes, enforce limits

```yaml
maxLogFilters: 50           # Max active log filters per network
maxAddresses: 10            # Max addresses per filter
maxTopics: 4                # Max topic arrays (Ethereum standard)
maxTopicValues: 10          # Max values per topic array
```

**Rationale:**
- Prevents abuse
- Protects upstream providers
- Reasonable limits for legitimate use cases
- Can be adjusted per network

### 12.8 Notification Order Guarantees

**Question:** Should notifications be ordered?

**Decision:** Best-effort ordering, no strict guarantees

**Rationale:**
- Blockchain events are already eventually consistent
- Strict ordering adds complexity and latency
- Clients should handle out-of-order events
- Block numbers provide ordering information

### 12.9 Reorg Handling

**Question:** How to handle blockchain reorganizations?

**Decision:** Phase 4 enhancement

**Approach:**
1. Detect reorgs by comparing parent hashes
2. Send special "reorg detected" notification (optional)
3. Continue sending correct chain events
4. Document reorg behavior for clients

**Rationale:**
- Reorgs are rare
- Most clients can handle via block numbers
- Advanced feature, not critical for MVP

---

## 13. Security Considerations

### 13.1 Authentication

**Strategy:** Reuse existing eRPC authentication

- WebSocket upgrade request goes through same auth as HTTP
- Auth strategies (JWT, Secret, SIWE, Network) work transparently
- No separate WebSocket authentication needed

### 13.2 Rate Limiting

**Strategy:** Apply existing rate limiters

```yaml
rateLimiters:
  budgets:
    - id: ws-budget
      rules:
        - method: "eth_subscribe"
          maxCount: 10
          period: 1m
        - method: "eth_unsubscribe"
          maxCount: 50
          period: 1m
```

### 13.3 Resource Limits

| Limit | Default | Purpose |
|-------|---------|---------|
| Max connections per network | 10,000 | Prevent connection exhaustion |
| Max subscriptions per connection | 100 | Prevent subscription abuse |
| Max log filters | 50 | Prevent query complexity attacks |
| Max addresses per filter | 10 | Prevent bloom filter abuse |
| Max message size | 1 MB | Prevent memory exhaustion |
| Connection idle timeout | 5 minutes | Clean up stale connections |

### 13.4 Input Validation

**Validate all client inputs:**

```go
// Subscription type
if subType != "newHeads" && subType != "logs" {
    return error("Invalid subscription type")
}

// Log filter addresses
for _, addr := range filter.Addresses {
    if !isValidAddress(addr) {
        return error("Invalid address format")
    }
}

// Log filter topics
if len(filter.Topics) > 4 {
    return error("Too many topic arrays")
}

for _, topicArr := range filter.Topics {
    if len(topicArr) > 10 {
        return error("Too many topic values")
    }
}
```

### 13.5 DoS Prevention

**Strategies:**

1. **Connection limits:** Per-network caps
2. **Subscription limits:** Per-connection caps
3. **Rate limiting:** Reuse existing limiters
4. **Filter complexity:** Validate and limit
5. **Idle timeout:** Close inactive connections
6. **Backpressure:** Drop notifications if client is slow

### 13.6 Data Privacy

**Considerations:**

- WebSocket connections use same TLS as HTTPS
- No additional PII collected
- Subscription filters may contain sensitive addresses (log but redact)
- Connection metadata (IP, user agent) follows existing privacy policy

---

## 14. References

### 14.1 Ethereum Standards

- [EIP-234: eth_subscribe/eth_unsubscribe](https://github.com/ethereum/EIPs/issues/234)
- [JSON-RPC 2.0 Specification](https://www.jsonrpc.org/specification)

### 14.2 Similar Implementations

- **venn** (gfx-labs): https://github.com/gfx-labs/venn
  - Multi-chain RPC load balancer
  - Redis-backed headstore for pub/sub
  - Leader election for stalker pattern
  
- **nodecore** (drpc): https://github.com/drpcorg/nodecore
  - Direct WebSocket connections to upstreams
  - Simpler architecture

### 14.3 WebSocket Standards

- [RFC 6455: The WebSocket Protocol](https://datatracker.ietf.org/doc/html/rfc6455)
- [gorilla/websocket Documentation](https://pkg.go.dev/github.com/gorilla/websocket)

### 14.4 Internal Documentation

- eRPC Architecture: `README.md`
- Configuration Guide: `docs/config/`
- Failsafe Policies: `upstream/failsafe.go`

---

## Appendix A: Message Flow Examples

### Example 1: Subscribe to NewHeads

**Client Request:**
```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "eth_subscribe",
  "params": ["newHeads"]
}
```

**Server Response:**
```json
{
  "jsonrpc": "2.0",
  "id": 1,
  "result": "0x1a2b3c4d5e6f7890abcdef1234567890"
}
```

**Notification (when new block arrives):**
```json
{
  "jsonrpc": "2.0",
  "method": "eth_subscription",
  "params": {
    "subscription": "0x1a2b3c4d5e6f7890abcdef1234567890",
    "result": {
      "number": "0x1234567",
      "hash": "0xabcdef...",
      "parentHash": "0x123456...",
      "timestamp": "0x65abc123",
      "gasLimit": "0x1c9c380",
      "gasUsed": "0x98a7b2",
      "miner": "0x742d35Cc...",
      "transactionsRoot": "0x56e81f...",
      "stateRoot": "0x789abc...",
      "receiptsRoot": "0xdef123..."
    }
  }
}
```

### Example 2: Subscribe to Logs

**Client Request:**
```json
{
  "jsonrpc": "2.0",
  "id": 2,
  "method": "eth_subscribe",
  "params": [
    "logs",
    {
      "address": "0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48",
      "topics": [
        "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"
      ]
    }
  ]
}
```

**Server Response:**
```json
{
  "jsonrpc": "2.0",
  "id": 2,
  "result": "0x9876543210fedcba0987654321fedcba"
}
```

**Notification (when matching log appears):**
```json
{
  "jsonrpc": "2.0",
  "method": "eth_subscription",
  "params": {
    "subscription": "0x9876543210fedcba0987654321fedcba",
    "result": {
      "address": "0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48",
      "topics": [
        "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef",
        "0x000000000000000000000000742d35Cc6634C0532925a3b844Bc9e7595f0bEb0",
        "0x00000000000000000000000012345678901234567890123456789012345678AB"
      ],
      "data": "0x0000000000000000000000000000000000000000000000000000000005f5e100",
      "blockNumber": "0x1234568",
      "transactionHash": "0xabcdef1234567890...",
      "transactionIndex": "0x12",
      "blockHash": "0x789abc...",
      "logIndex": "0x3",
      "removed": false
    }
  }
}
```

### Example 3: Unsubscribe

**Client Request:**
```json
{
  "jsonrpc": "2.0",
  "id": 3,
  "method": "eth_unsubscribe",
  "params": ["0x1a2b3c4d5e6f7890abcdef1234567890"]
}
```

**Server Response:**
```json
{
  "jsonrpc": "2.0",
  "id": 3,
  "result": true
}
```

### Example 4: Error Response

**Client Request (invalid params):**
```json
{
  "jsonrpc": "2.0",
  "id": 4,
  "method": "eth_subscribe",
  "params": ["invalidType"]
}
```

**Server Response:**
```json
{
  "jsonrpc": "2.0",
  "id": 4,
  "error": {
    "code": -32602,
    "message": "Invalid params",
    "data": "Invalid subscription type: invalidType. Supported types: newHeads, logs"
  }
}
```

---

## Appendix B: Configuration Examples

### Minimal Configuration

```yaml
server:
  httpPort: 4000
  websocket:
    enabled: true

projects:
  - id: main
    networks:
      - architecture: evm
        evm:
          chainId: 1
    upstreams:
      - id: alchemy
        endpoint: https://eth-mainnet.g.alchemy.com/v2/YOUR_KEY
```

### Full Configuration

```yaml
logLevel: info

server:
  httpPort: 4000
  
  websocket:
    enabled: true
    maxConnectionsPerNetwork: 10000
    maxSubscriptionsPerConnection: 100
    pingInterval: 30s
    pongTimeout: 60s
    readBufferSize: 8192
    writeBufferSize: 8192

projects:
  - id: main
    
    networks:
      # Ethereum Mainnet
      - architecture: evm
        evm:
          chainId: 1
        subscription:
          pollInterval: 2s
          maxLogFilters: 50
      
      # Arbitrum (faster blocks)
      - architecture: evm
        evm:
          chainId: 42161
        subscription:
          pollInterval: 500ms
          maxLogFilters: 100
    
    upstreams:
      - id: alchemy-eth
        endpoint: https://eth-mainnet.g.alchemy.com/v2/YOUR_KEY
      
      - id: infura-eth
        endpoint: https://mainnet.infura.io/v3/YOUR_KEY
      
      - id: alchemy-arb
        endpoint: https://arb-mainnet.g.alchemy.com/v2/YOUR_KEY

rateLimiters:
  budgets:
    - id: ws-budget
      rules:
        - method: "eth_subscribe"
          maxCount: 10
          period: 1m
        - method: "eth_unsubscribe"
          maxCount: 50
          period: 1m
```

---

**Document Status:** Draft  
**Last Updated:** 2025-01-XX  
**Next Review:** After Phase 1 implementation

---

