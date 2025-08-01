package handlers

import (
    "context"
    "crypto/rand"
    "encoding/hex"
    "encoding/json"
    "fmt"
    "GO-OPENAI/config"
	"GO-OPENAI/models"
	"GO-OPENAI/utils"
    "net/http"
    "sync"
    "time"
    
    "github.com/gin-gonic/gin"
    "github.com/gorilla/websocket"
)

type WebSocketHandler struct {
    config     *config.Config
    logger     *utils.Logger
    workerPool *utils.WorkerPool
    
    // Connection management
    connManager *models.ConnectionManager
    wsClients   map[string]*websocket.Conn
    clientsMux  sync.RWMutex
    
    // WebSocket upgrader
    upgrader websocket.Upgrader
    
    // Services
    llmService    *services.LLMService
    speechService *services.SpeechService
    ttsService    *services.TTSService
    
    // System metrics
    metrics *models.SystemMetrics
    
    // Context management
    contextMgr *utils.ContextManager
}

// Constructor that accepts service instances
func NewWebSocketHandlerWithServices(
    cfg *config.Config, 
    logger *utils.Logger, 
    workerPool *utils.WorkerPool,
    llmService *services.LLMService,
    speechService *services.SpeechService,
    ttsService *services.TTSService,
) *WebSocketHandler {
    return &WebSocketHandler{
        config:      cfg,
        logger:      logger,
        workerPool:  workerPool,
        connManager: models.NewConnectionManager(cfg.Server.MaxConnections),
        wsClients:   make(map[string]*websocket.Conn),
        upgrader: websocket.Upgrader{
            CheckOrigin: func(r *http.Request) bool {
                return true // Need to check origin in production
            },
            ReadBufferSize:  1024,
            WriteBufferSize: 1024,
        },
        llmService:    llmService,
        speechService: speechService,
        ttsService:    ttsService,
        metrics:       models.NewSystemMetrics(),
        contextMgr:    utils.NewContextManager(logger),
    }
}

// WebSocket connection handler
func (h *WebSocketHandler) HandleWebSocket(c *gin.Context) {
    // Upgrade HTTP connection to WebSocket
    conn, err := h.upgrader.Upgrade(c.Writer, c.Request, nil)
    if err != nil {
        h.logger.WithError(err).Error("WebSocket upgrade failed")
        return
    }
    defer conn.Close()
    
    // Generate connection ID
    connID := h.generateConnectionID()
    
    // Create connection object
    connection := &models.Connection{
        ID:        connID,
        CreatedAt: time.Now(),
        LastSeen:  time.Now(),
        UserAgent: c.Request.UserAgent(),
        ClientIP:  c.ClientIP(),
    }
    
    // Add to connection manager
    if !h.connManager.AddConnection(connection) {
        h.logger.Error("Connection limit reached")
        h.sendSystemMessage(conn, "error", "Server connection limit reached")
        return
    }
    defer h.connManager.RemoveConnection(connID)
    
    // Add to WebSocket client list
    h.clientsMux.Lock()
    h.wsClients[connID] = conn
    h.clientsMux.Unlock()
    defer func() {
        h.clientsMux.Lock()
        delete(h.wsClients, connID)
        h.clientsMux.Unlock()
    }()
    
    h.logger.WithFields(map[string]interface{}{
        "connID":    connID,
        "clientIP":  connection.ClientIP,
        "userAgent": connection.UserAgent,
    }).Info("New WebSocket connection established")
    
    // Send connection success message
    h.sendSystemMessage(conn, "connected", "Connection established")
    
    // Set read/write timeouts
    conn.SetReadDeadline(time.Now().Add(60 * time.Second))
    conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
    
    // Set pong handler (heartbeat detection)
    conn.SetPongHandler(func(string) error {
        conn.SetReadDeadline(time.Now().Add(60 * time.Second))
        return nil
    })
    
    // Start heartbeat goroutine
    go h.heartbeat(conn, connID)
    
    // Message processing loop
    for {
        var message models.WSMessage
        err := conn.ReadJSON(&message)
        if err != nil {
            if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
                h.logger.WithError(err).Error("WebSocket connection closed unexpectedly")
            }
            break
        }
        
        // Update last activity time
        if conn, exists := h.connManager.GetConnection(connID); exists {
            conn.LastSeen = time.Now()
        }
        
        // Reset read timeout
        conn.SetReadDeadline(time.Now().Add(60 * time.Second))
        
        // Submit message processing task to worker pool
        h.workerPool.Submit(&utils.Job{
            ID: fmt.Sprintf("ws-%s-%d", connID, time.Now().UnixNano()),
            Handler: func(ctx context.Context) error {
                return h.handleMessage(ctx, conn, connID, &message)
            },
            Priority: utils.PriorityNormal,
        })
    }
    
    h.logger.WithField("connID", connID).Info("WebSocket connection closed")
}

// Message processing
func (h *WebSocketHandler) handleMessage(ctx context.Context, conn *websocket.Conn, connID string, message *models.WSMessage) error {
    startTime := time.Now()
    h.metrics.IncrementRequests()
    
    h.logger.WithFields(map[string]interface{}{
        "connID":      connID,
        "messageType": message.Type,
        "messageID":   message.ID,
    }).Debug("Processing WebSocket message")
    
    defer func() {
        duration := time.Since(startTime)
        h.metrics.UpdateResponseTime(duration)
    }()
    
    switch message.Type {
    case "voice":
        return h.handleVoiceMessage(ctx, conn, connID, message)
    case "text":
        return h.handleTextMessage(ctx, conn, connID, message)
    case "ping":
        return h.handlePingMessage(conn, connID)
    case "system":
        return h.handleSystemMessage(ctx, conn, connID, message)
    default:
        h.logger.WithField("messageType", message.Type).Warn("Unknown message type")
        return h.sendErrorMessage(conn, "unknown_message_type", "Unknown message type")
    }
}

// Handle voice message
func (h *WebSocketHandler) handleVoiceMessage(ctx context.Context, conn *websocket.Conn, connID string, message *models.WSMessage) error {
    h.logger.WithField("connID", connID).Info("Processing voice message")
    
    // Parse voice message
    voiceData, ok := message.Content.(map[string]interface{})
    if !ok {
        return h.sendErrorMessage(conn, "invalid_voice_data", "Invalid voice data format")
    }
    
    audioData, ok := voiceData["audio_data"].(string)
    if !ok || audioData == "" {
        return h.sendErrorMessage(conn, "missing_audio_data", "Missing audio data")
    }
    
    // Send processing status
    h.sendSystemMessage(conn, "processing", "Processing voice...")
    
    // Create processing context
    processCtx, cancel := h.contextMgr.WithTimeout(ctx, time.Duration(h.config.Server.RequestTimeout)*time.Second)
    defer cancel()
    
    // 1. Speech recognition (ASR)
    h.sendSystemMessage(conn, "processing", "Recognizing speech...")
    recognizedText, err := h.speechService.SpeechToTextWithContext(processCtx, audioData)
    if err != nil {
        h.metrics.IncrementFailed()
        h.logger.WithError(err).Error("Speech recognition failed")
        return h.sendErrorMessage(conn, "speech_recognition_failed", fmt.Sprintf("Speech recognition failed: %v", err))
    }
    
    // Send recognition result
    h.sendMessage(conn, "speech_recognized", map[string]interface{}{
        "text": recognizedText,
    })
    
    if recognizedText == "" {
        return h.sendSystemMessage(conn, "completed", "No speech content recognized")
    }
    
    // 2. Text processing (LLM)
    h.sendSystemMessage(conn, "processing", "Generating response...")
    aiResponse, err := h.llmService.ProcessTextWithContext(processCtx, recognizedText)
    if err != nil {
        h.metrics.IncrementFailed()
        h.logger.WithError(err).Error("Text processing failed")
        return h.sendErrorMessage(conn, "text_processing_failed", fmt.Sprintf("Text processing failed: %v", err))
    }
    
    // Send AI response text
    h.sendMessage(conn, "ai_response", map[string]interface{}{
        "text":         aiResponse.Text,
        "confidence":   aiResponse.Confidence,
        "process_time": aiResponse.ProcessTime,
        "tokens_used":  aiResponse.TokensUsed,
    })
    
    // 3. Text-to-speech (TTS)
    h.sendSystemMessage(conn, "processing", "Generating speech...")
    audioURL, err := h.ttsService.TextToSpeechWithContext(processCtx, aiResponse.Text)
    if err != nil {
        h.logger.WithError(err).Error("Text-to-speech failed")
        // TTS failure is not fatal, user already received text response
        h.sendSystemMessage(conn, "warning", "Text-to-speech failed, but text response generated")
    } else {
        // Send complete AI response (including audio)
        aiResponse.AudioURL = audioURL
        h.sendMessage(conn, "ai_response_complete", map[string]interface{}{
            "text":         aiResponse.Text,
            "audio_url":    aiResponse.AudioURL,
            "confidence":   aiResponse.Confidence,
            "process_time": aiResponse.ProcessTime,
            "tokens_used":  aiResponse.TokensUsed,
        })
    }
    
    h.metrics.IncrementSuccessful()
    h.sendSystemMessage(conn, "completed", "Processing completed")
    
    return nil
}

// Handle text message
func (h *WebSocketHandler) handleTextMessage(ctx context.Context, conn *websocket.Conn, connID string, message *models.WSMessage) error {
    h.logger.WithField("connID", connID).Info("Processing text message")
    
    // Parse text message
    textData, ok := message.Content.(map[string]interface{})
    if !ok {
        return h.sendErrorMessage(conn, "invalid_text_data", "Invalid text data format")
    }
    
    userText, ok := textData["text"].(string)
    if !ok || userText == "" {
        return h.sendErrorMessage(conn, "missing_text", "Missing text content")
    }
    
    // Send processing status
    h.sendSystemMessage(conn, "processing", "Processing text...")
    
    // Create processing context
    processCtx, cancel := h.contextMgr.WithTimeout(ctx, time.Duration(h.config.Server.RequestTimeout)*time.Second)
    defer cancel()
    
    // Text processing (LLM)
    aiResponse, err := h.llmService.ProcessTextWithContext(processCtx, userText)
    if err != nil {
        h.metrics.IncrementFailed()
        h.logger.WithError(err).Error("Text processing failed")
        return h.sendErrorMessage(conn, "text_processing_failed", fmt.Sprintf("Text processing failed: %v", err))
    }
    
    // Send AI response text
    h.sendMessage(conn, "ai_response", map[string]interface{}{
        "text":         aiResponse.Text,
        "confidence":   aiResponse.Confidence,
        "process_time": aiResponse.ProcessTime,
        "tokens_used":  aiResponse.TokensUsed,
    })
    
    // Check if TTS is needed (optional)
    needTTS, _ := textData["need_tts"].(bool)
    if needTTS {
        h.sendSystemMessage(conn, "processing", "Generating speech...")
        audioURL, err := h.ttsService.TextToSpeechWithContext(processCtx, aiResponse.Text)
        if err != nil {
            h.logger.WithError(err).Error("Text-to-speech failed")
            h.sendSystemMessage(conn, "warning", "Text-to-speech failed")
        } else {
            aiResponse.AudioURL = audioURL
            h.sendMessage(conn, "ai_response_complete", map[string]interface{}{
                "text":         aiResponse.Text,
                "audio_url":    aiResponse.AudioURL,
                "confidence":   aiResponse.Confidence,
                "process_time": aiResponse.ProcessTime,
                "tokens_used":  aiResponse.TokensUsed,
            })
        }
    }
    
    h.metrics.IncrementSuccessful()
    h.sendSystemMessage(conn, "completed", "Processing completed")
    
    return nil
}

// Handle ping message
func (h *WebSocketHandler) handlePingMessage(conn *websocket.Conn, connID string) error {
    return h.sendMessage(conn, "pong", map[string]interface{}{
        "timestamp": time.Now().Unix(),
    })
}

// Handle system message
func (h *WebSocketHandler) handleSystemMessage(ctx context.Context, conn *websocket.Conn, connID string, message *models.WSMessage) error {
    systemData, ok := message.Content.(map[string]interface{})
    if !ok {
        return h.sendErrorMessage(conn, "invalid_system_data", "Invalid system message format")
    }
    
    action, ok := systemData["action"].(string)
    if !ok {
        return h.sendErrorMessage(conn, "missing_action", "Missing action type")
    }
    
    switch action {
    case "get_status":
        return h.handleGetStatus(conn, connID)
    case "get_metrics":
        return h.handleGetMetrics(conn, connID)
    case "get_services_stats":
        return h.handleGetServicesStats(conn, connID)
    default:
        return h.sendErrorMessage(conn, "unknown_action", "Unknown system action")
    }
}

// Get status
func (h *WebSocketHandler) handleGetStatus(conn *websocket.Conn, connID string) error {
    status := map[string]interface{}{
        "server_status":    "running",
        "connection_id":    connID,
        "active_connections": h.connManager.GetCount(),
        "services": map[string]interface{}{
            "llm":    "running",
            "speech": "running", 
            "tts":    "running",
        },
        "timestamp": time.Now().Unix(),
    }
    
    return h.sendMessage(conn, "status", status)
}

// Get metrics
func (h *WebSocketHandler) handleGetMetrics(conn *websocket.Conn, connID string) error {
    metrics := h.metrics.GetMetrics()
    metricsData := map[string]interface{}{
        "total_requests":       metrics.TotalRequests,
        "successful_requests":  metrics.SuccessfulRequests,
        "failed_requests":      metrics.FailedRequests,
        "average_response_time": metrics.AverageResponseTime.Milliseconds(),
        "active_connections":   metrics.ActiveConnections,
        "worker_pool_stats":    metrics.WorkerPoolStats,
        "timestamp":            time.Now().Unix(),
    }
    
    return h.sendMessage(conn, "metrics", metricsData)
}

// Get service statistics
func (h *WebSocketHandler) handleGetServicesStats(conn *websocket.Conn, connID string) error {
    servicesStats := map[string]interface{}{
        "llm_service":    h.llmService.GetStats(),
        "speech_service": h.speechService.GetStats(),
        "tts_service":    h.ttsService.GetStats(),
        "timestamp":      time.Now().Unix(),
    }
    
    return h.sendMessage(conn, "services_stats", servicesStats)
}

// HTTP text chat API
func (h *WebSocketHandler) HandleTextChat(c *gin.Context) {
    startTime := time.Now()
    h.metrics.IncrementRequests()
    
    var request struct {
        Text    string `json:"text" binding:"required"`
        NeedTTS bool   `json:"need_tts"`
    }
    
    if err := c.ShouldBindJSON(&request); err != nil {
        h.metrics.IncrementFailed()
        c.JSON(http.StatusBadRequest, gin.H{
            "error": "Invalid request format",
            "details": err.Error(),
        })
        return
    }
    
    // Create processing context
    ctx, cancel := h.contextMgr.WithTimeout(c.Request.Context(), 
        time.Duration(h.config.Server.RequestTimeout)*time.Second)
    defer cancel()
    
    // Text processing
    aiResponse, err := h.llmService.ProcessTextWithContext(ctx, request.Text)
    if err != nil {
        h.metrics.IncrementFailed()
        h.logger.WithError(err).Error("Text processing failed")
        c.JSON(http.StatusInternalServerError, gin.H{
            "error": "Text processing failed",
            "details": err.Error(),
        })
        return
    }
    
    // Optional text-to-speech
    if request.NeedTTS {
        audioURL, err := h.ttsService.TextToSpeechWithContext(ctx, aiResponse.Text)
        if err != nil {
            h.logger.WithError(err).Error("Text-to-speech failed")
            // TTS failure doesn't affect text response
        } else {
            aiResponse.AudioURL = audioURL
        }
    }
    
    h.metrics.IncrementSuccessful()
    h.metrics.UpdateResponseTime(time.Since(startTime))
    
    c.JSON(http.StatusOK, gin.H{
        "success": true,
        "data": aiResponse,
    })
}

// Status API
func (h *WebSocketHandler) HandleStatus(c *gin.Context) {
    status := map[string]interface{}{
        "server_status":      "running",
        "active_connections": h.connManager.GetCount(),
        "services": map[string]interface{}{
            "llm":    "running",
            "speech": "running",
            "tts":    "running",
        },
        "timestamp": time.Now().Unix(),
    }
    
    c.JSON(http.StatusOK, gin.H{
        "success": true,
        "data": status,
    })
}

// Metrics API
func (h *WebSocketHandler) HandleMetrics(c *gin.Context) {
    metrics := h.metrics.GetMetrics()
    servicesStats := map[string]interface{}{
        "llm_service":    h.llmService.GetStats(),
        "speech_service": h.speechService.GetStats(),
        "tts_service":    h.ttsService.GetStats(),
    }
    
    c.JSON(http.StatusOK, gin.H{
        "success": true,
        "data": map[string]interface{}{
            "system_metrics":  metrics,
            "services_stats":  servicesStats,
            "timestamp":       time.Now().Unix(),
        },
    })
}

// Heartbeat detection
func (h *WebSocketHandler) heartbeat(conn *websocket.Conn, connID string) {
    ticker := time.NewTicker(30 * time.Second)
    defer ticker.Stop()
    
    for range ticker.C {
        if err := conn.WriteMessage(websocket.PingMessage, nil); err != nil {
            h.logger.WithFields(map[string]interface{}{
                "connID": connID,
                "error":  err,
            }).Debug("Heartbeat send failed, connection may be closed")
            return
        }
    }
}

// Utility methods

// Generate connection ID
func (h *WebSocketHandler) generateConnectionID() string {
    bytes := make([]byte, 8)
    rand.Read(bytes)
    return hex.EncodeToString(bytes)
}

// Send message
func (h *WebSocketHandler) sendMessage(conn *websocket.Conn, msgType string, content interface{}) error {
    message := models.WSMessage{
        Type:      msgType,
        Content:   content,
        Timestamp: time.Now(),
    }
    
    conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
    return conn.WriteJSON(message)
}

// Send system message
func (h *WebSocketHandler) sendSystemMessage(conn *websocket.Conn, status, message string) error {
    return h.sendMessage(conn, "system", models.SystemStatus{
        Connected: true,
        Status:    status,
        Message:   message,
    })
}

// Send error message
func (h *WebSocketHandler) sendErrorMessage(conn *websocket.Conn, errorType, message string) error {
    return h.sendMessage(conn, "error", map[string]interface{}{
        "error_type": errorType,
        "message":    message,
        "timestamp":  time.Now().Unix(),
    })
}

// Broadcast message to all connections
func (h *WebSocketHandler) BroadcastMessage(msgType string, content interface{}) {
    h.clientsMux.RLock()
    defer h.clientsMux.RUnlock()
    
    message := models.WSMessage{
        Type:      msgType,
        Content:   content,
        Timestamp: time.Now(),
    }
    
    for connID, conn := range h.wsClients {
        go func(connID string, conn *websocket.Conn) {
            if err := conn.WriteJSON(message); err != nil {
                h.logger.WithFields(map[string]interface{}{
                    "connID": connID,
                    "error":  err,
                }).Error("Broadcast message failed")
            }
        }(connID, conn)
    }
}