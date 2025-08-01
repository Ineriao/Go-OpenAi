package models

import (
	"sync"
	"time"
)

// AI Response
type AIResponse struct {
	Text		string	`json:"text"`
	AudioURL	string	`json:"audio_url"`
	Confidence  float64 `json:"confidence"`
    ProcessTime int64   `json:"process_time"` 
    TokensUsed  int     `json:"tokens_used"`
}

type WSMessage struct {
    Type      string      `json:"type"`
    Content   interface{} `json:"content"`
    Timestamp time.Time   `json:"timestamp"`
    ID        string      `json:"id,omitempty"`
}

type VoiceMessage struct {
    AudioData string `json:"audio_data"`
    Format    string `json:"format"`
    Duration  int    `json:"duration"`
}

type TextMessage struct {
    Text   string `json:"text"`
    Sender string `json:"sender"`
}

type SystemStatus struct {
    Connected bool   `json:"connected"`
    Status    string `json:"status"`
    Message   string `json:"message"`
}

type ConnectionManager struct {
    connections map[string]*Connection
    mutex       sync.RWMutex
    maxConns    int
}

type Connection struct {
    ID        string
    CreatedAt time.Time
    LastSeen  time.Time
    UserAgent string
    ClientIP  string
    mutex     sync.RWMutex
}

func NewConnectionManager(maxConns int) *ConnectionManager {
    return &ConnectionManager{
        connections: make(map[string]*Connection),
        maxConns:    maxConns,
    }
}

func (cm *ConnectionManager) AddConnection(conn *Connection) bool {
    cm.mutex.Lock()
    defer cm.mutex.Unlock()
    
    if len(cm.connections) >= cm.maxConns {
        return false
    }
    
    cm.connections[conn.ID] = conn
    return true
}

func (cm *ConnectionManager) RemoveConnection(id string) {
    cm.mutex.Lock()
    defer cm.mutex.Unlock()
    delete(cm.connections, id)
}

func (cm *ConnectionManager) GetConnection(id string) (*Connection, bool) {
    cm.mutex.RLock()
    defer cm.mutex.RUnlock()
    conn, exists := cm.connections[id]
    return conn, exists
}

func (cm *ConnectionManager) GetCount() int {
    cm.mutex.RLock()
    defer cm.mutex.RUnlock()
    return len(cm.connections)
}

type SystemMetrics struct {
    ActiveConnections   int           `json:"active_connections"`
    TotalRequests       int64         `json:"total_requests"`
    SuccessfulRequests  int64         `json:"successful_requests"`
    FailedRequests      int64         `json:"failed_requests"`
    AverageResponseTime time.Duration `json:"average_response_time"`
    WorkerPoolStats     WorkerStats   `json:"worker_pool_stats"`
    mutex               sync.RWMutex
}

type WorkerStats struct {
    ActiveWorkers int   `json:"active_workers"`
    QueuedJobs    int   `json:"queued_jobs"`
    CompletedJobs int64 `json:"completed_jobs"`
}

func NewSystemMetrics() *SystemMetrics {
    return &SystemMetrics{}
}

func (sm *SystemMetrics) IncrementRequests() {
    sm.mutex.Lock()
    defer sm.mutex.Unlock()
    sm.TotalRequests++
}

func (sm *SystemMetrics) IncrementSuccessful() {
    sm.mutex.Lock()
    defer sm.mutex.Unlock()
    sm.SuccessfulRequests++
}

func (sm *SystemMetrics) IncrementFailed() {
    sm.mutex.Lock()
    defer sm.mutex.Unlock()
    sm.FailedRequests++
}

func (sm *SystemMetrics) UpdateResponseTime(duration time.Duration) {
    sm.mutex.Lock()
    defer sm.mutex.Unlock()
    if sm.AverageResponseTime == 0 {
        sm.AverageResponseTime = duration
    } else {
        sm.AverageResponseTime = (sm.AverageResponseTime + duration) / 2
    }
}

func (sm *SystemMetrics) GetMetrics() SystemMetrics {
    sm.mutex.RLock()
    defer sm.mutex.RUnlock()
    return *sm
}

































