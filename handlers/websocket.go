package handlers

import (
	"GO-OPENAI/config"
	"GO-OPENAI/models"
	"GO-OPENAI/utils"
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang/protobuf/ptypes/duration"
	"github.com/gorilla/websocket"
	"golang.org/x/text/cases"
)

type WebSocketHandler struct {
	config		*config.Config
	logger		*utils.Logger
	workerPool	*utils.WorkerPool

	//connect manager
	connManager *models.ConnectionManager
	wsClients	map[string]*websocket.Conn
	clientsMux	sync.RWMutex

	// WebSocket Upgrader
	upgrader	websocket.Upgrader

	// services
	llmService	*services.LLMService
	asrService	*services.ASRService
	ttsService	*services.TTSService

	// metrics
	metrics		*models.SystemMetrics

	// context
	contextMgr	*utils.ContextManager
}

func NewWebSockectHandler(cfg *config.Config, logger *utils.Logger, workerPool *utils.WorkerPool) *WebSocketHandler {
	return &WebSocketHandler{
		config: 		cfg,
		logger: 		logger,
		workerPool: 	workerPool,
		connManager:	models.NewConnectionManager(cfg.Server.MaxConnections),
		wsClients:		make(map[string]*websocket.Conn),
		upgrader:		websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
			ReadBufferSize: 1024,
			WriteBufferSize: 1024,
		},
		llmService:  	services.NewLLMService(&cfg.AI,	logger),
		asrService:  	services.NewASRService(&cfg.Audio, logger),
		ttsService: 	services.NewTTSService(&cfg.Audio, logger),

		metrics: 		models.NewSystemMetrics(),
		contextMgr:		utils.NewContextManager(logger),

	}
}

func (h *WebSocketHandler) HandleWebSocket(c *gin.Context) {
	// Check connect limit
	if h.connManager.GetCount() >= h.config.Server.MaxConnections {
		h.logger.Warn("The maximum number of connections has been reached")
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "服务器繁忙，请稍后重试"})
		return
	}

	conn, err := h.upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		h.logger.WithError(err).Error(("WebSocket upgrade failed"))
		return
	}

	// Generate a connection ID
	connID := h.generateConnectionID()

	// Creating a connection object
	connection := &models.Connection{
		ID:			connID,
		CreatedAt:	time.Now(),
		LastSeen:	time.Now(),
		UserAgent:	c.GetHeader("User-Agent"),
		ClientIP:	c.ClientIP(),
	}

	// Add Connection Manager
	if !h.connManager.AddConnection(connection) {
		h.logger.Error("Unable to add new connection")
		conn.Close()
		return
	}

	// Registering WebSocket Client
	h.clientsMux.Lock()
	h.wsClients[connID] = conn
	h.clientsMux.Unlock()

	h.logger.WithFields(map[string]interface{}{
		"connID":		connID,
		"clientIP":		connection.clientIP,
		"userAgent":	connection.userAgent,
		"totalConns":	h.connManager.GetCount(),
	}).Info("New client connection")

	// Update metrics
	h.metrics.ActiveConnections = h.connManager.GetCount()

	// SendMessage
	h.sendMessage(conn, "system", models.SystemStatus{
		Connected: 	true,
		Status:		"connected",
		Message:	"Connection successful",
	})

	// Create context
	ctx, cancel := h.contextMgr.WithCancel(context.Background())
	defer cancel()

	// Heartbeat
	go h.heartbeat(ctx, conn, connID)

	// Processing Messages
	h.handleMessage(ctx, conn, connID)

	// Clean clilent
	h.cleanup(connID, conn)
}

func (h *WebSocketHandler) handleMessage(ctx context.Context, conn *websocket.Conn, connID string) {
	defer func ()  {
		if r := recover(); r != nil {
			h.logger.WithFields(map[string]interface{}{
				"connID":	connID,
				"panic":	r,
			}).Error("WebSocket message processing panics")
		}
	}()

	// Set read ddl
	conn.SetReadDeadline(time.Now().Add(60 * time.Second))

	// Set pong
	conn.SetPongHandler(func (string) error {
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		h.updateLastSeen(connID)
		return nil
	})

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		var wsMsg models.WSMessage
		err := conn.ReadJSON(&wsMsg)
		if err != nil {
			h.logger.WithFields(map[string]interface{}{
				"connID":	connID,
				"error":	err,
			}).Debug("Faild to read Websocket message")
			return
		}

		// Update last seen time
		h.updateLastSeen(connID)

		h.logger.WithFields(map[string]interface{}{
			"connID":		connID,
			"messageType":	wsMsg.Type,
		}).Debug("Receive WebSocket message")

		// Submit to coroutine pool
		job := 	&utils.SimpleJob{
			ID:				h.generateJob(connID, wsMsg.Type),
			Priority: 		h.getMessagePriority(wsMsg.Type),
			Fn:	func (jobCtx context.Context) error {
				return h.processMessage(jobCtx, conn, connID, wsMsg)
			},
		}

		// Submit timeouts
		if err := h.workerPool.SubmitWithTimeout(job, 5*time.Second); err != nil {
			h.logger.WithFields(map[string]interface{}{
				"connID":		connID,
				"error":		err,
			}).Error("Submit faild")

			h.sendMessage(conn, "error", "服务器繁忙，请稍后再试")
		}
	}
}

func (h *WebSocketHandler) processMessage(ctx context.Context, conn *websocket.Conn, connID string, wsMsg models.WSMessage) error {
	startTime := time.Now()
	h.metrics.IncrementRequests()

	defer func ()  {
		duration := time.Since(startTime)
		h.metrics.UpdateResponseTime(duration)
	}()

	// Create timeout context
	msgCtx, cancel := h.contextMgr.WithTimeout(ctx, time.Duration(h.config.Server.RequestTimeout)*time.Second)
	defer cancel()

	var err error

	// Handling different types of messages
	switch	wsMsg.Type {
	case "voice":
		err = h.handleVoiceMessage(msgCtx, conn, connID, wsMsg)
	case "text" :
		err = h.handleTextMessage(msgCtx, conn, connID, wsMsg)
	case "ping" :
		err = h.sendMessage(conn, "pong", "pong")
	default:
		h.logger.WithFields(map[string]interface{}{
			"connID":		connID,
			"messageType":	wsMsg.Type,
		}).Warn("Unknown message type")
		err = h.sendMessage(conn, "error", "未知消息类型")
	}

	if err != nil {
		h.metrics.IncrementFailed()
		return err
	}

	h.metrics.IncrementSuccessful()
	return nil
}

func (h *WebSocketHandler) handleVoiceMessage(ctx context.Context, conn *websocket.Conn, connID string, wsMsg models.WSMessage) error {
	// Parsing voice messages
	voiceData, ok := wsMsg.Content.(map[string]interface{})
	if !ok {
		h.logger.WithField("connID", connID).Error("Invalid voice data format")
		return h.sendMessage(conn, "error", "无效的语音数据格式")
	}

	h.logger.WithFields(map[string]interface{}{
		"connID":			connID,
		"audioDataLength":	len(audioData),
	}).Info("Processing voice messages")

	// Speech-to-text
	text, err := h.asrService.SpeechToTextWithContext(ctx, audioData)
	if err != nil {
		h.logger.WithFields(map[string]interface{}{
			"connID":		connID,
			"err":			err,
		}).Error("Recognition failed")
		return h.sendMessage(conn, "error", "语音识别失败")
	}

	// Send result
	if err := h.sendMessage(conn, "asr_result", models.TextMessage{
		Text:		text,
		Sender:		"user",
	}); err != nil {
		return err
	}

	// Handling AI conversations
	return h.processAIChat(ctx, conn, connID, text)
}

func (h *WebSocketHandler) handleTextMessage(ctx context.Context, conn *websocket.Conn, connID string, wsMsg modles.WSMessage) error {
	textData, ok := wsMsg.Content.(map[string]interface{})
	if !ok {
		h.logger.WithField("connID", connID).Error("Invalid text data format")
		return h.sendMessage(conn, "error", "无效的数据格式")
	}

	text, ok := textData["text"].(string)
	if !ok {
		h.logger.WithField("connID", connID).Error("Missing text content")
		return h.sendMessage(conn, "error", "缺少文本内容")
	}

	h.logger.WithFields(map[string]interface{}{
		"connID":			connID,
		"text":				text,
	}).Info("Processing text messages")

	// AIChat
	return h.processAIChat(ctx, conn, connID, text)
}

func (h *WebSocketHandler) processAIChat(ctx context.Context, conn *websocket.Conn, connID string, userText string) error {
	h.logger.WithFields(map[string]interface{}{
		"connID":			connID,
		"userText":			userText,
	}).Info("Processing AI chat")

	// Concurrent processing of AI text generation and speech synthesis
	type aiResult struct {
		response	*models.AIResponse
		err			error
	}

	type ttsResult struct {
		audioURL	string
		err			error
	}	

	aiCh := make(chan aiResult, 1)
	ttsCh := make(chan ttsResult, 1)

	// Start AI goroutine
	go func ()  {
		resp, err := h.llmService.ProcessTextWithContext(ctx, userText)
		aiCh <- aiResult{response: resp, err: err}
	}()

	// Waiting AI respond
	var aiResp *models.AIResponse
	select {
	case result := <-aiCh:
		if result.err != nil {
			h.logger.WithFields(map[string]interface{}{
				"connID":		connID,
				"error":		result.err,
			}).Error("AI processing failed")
			return h.sendMessage(conn, "error", "AI服务暂时不可用")
		}
		aiResp = result.response
	case <-ctx.Done():
		return ctx.Err()
	}

	// Start TTS goroutine
	go func ()  {
		audioURL, err := h.ttsService.TextToSpeechWithContext(ctx, aiResp.Text)
		ttsCh <- ttsResult{audioURL: audioURL, err: err}
	}()

	// SendSend text response
	if err := h.sendMessage(conn, "ai_response", models.AIResponse{
		Text:			aiResp.Text,
		Confidence:		aiResp.Confidence,
		ProcessTime:	aiResp.ProcessTime,
	}); err != nil {
		return err
	}

	// Wait for TTS result and send audio URL
	select {
	case result := <- ttsCh:
		if result.err != nil {
			h.logger.WithFields(map[string]interface{}{
				"connID":		connID,
				"error":		result.err,
			}).Error("Speech synthesis failed")
		} else {
			aiResp.audioURL = result.audioURL
			// Send a complete response including the audio URL
			h.sendMessage(conn, "ai_response_complete", aiResp)
		}
	case <-ctx.Done():
		return ctx.Err()
	}

	h.logger.WithFields(map[string]interface{}{
		"connID":       connID,
        "responseText": aiResp.Text,
        "audioURL":     aiResp.AudioURL,
        "processTime":  aiResp.ProcessTime,
	}).Info("AI dialogue processing completed")

	return nil
}

