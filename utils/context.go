package utils

import (
	"context"
	"time"
)

// ContextManager 上下文管理
type ContextManager struct {
	logger *Logger
}

func NewContextManager(logger *Logger) *ContextManager {
	return &ContextManager{logger: logger}
}

// WithTimeout 创建带超时上下文
func (cm *ContextManager) WithTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(parent, timeout)

	cm.logger.WithField("timeout", timeout).Debug("Creating a timeout context")

	return ctx, cancel
}

// WithValue 创建带值上下文
func (cm *ContextManager) WithValue(parent context.Context, key, value interface{}) context.Context {
	ctx := context.WithValue(parent, key, value)

	cm.logger.WithFields(map[string]interface{}{
		"key":		key,
		"value":	value,
	}).Debug("Creating a context with values")

	return ctx
}