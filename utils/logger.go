package utils

import (
	"os"
	"github.com/sirupsen/logrus"
)


type Logger struct {
	*logrus.Logger
}

// NewLogger 创建logger实例
func NewLogger() *Logger {
	logger := logrus.New()

	logger.SetOutput(os.Stdout)
	
	logger.SetLevel(logrus.InfoLevel)
	logger.SetFormatter(&logrus.TextFormatter{
		FullTimestamp: true,
		TimestampFormat: "2006-01-02 15:04:05",
		ForceColors: true,
	})

	return &Logger{Logger: logger}
}

// SetLevel 设置日志级别
func (l *Logger) SetLevel(level string) {
	switch level {
	case "debug":
		l.Logger.SetLevel(logrus.DebugLevel)
	case "info":
		l.Logger.SetLevel(logrus.InfoLevel)
	case "warn":
		l.Logger.SetLevel(logrus.WarnLevel)
	case "error":
		l.Logger.SetLevel(logrus.ErrorLevel)
	default:
		l.Logger.SetLevel(logrus.InfoLevel)
	}
}

// SetJSONFormatter 日志格式
func(l *Logger) SetJSONFormatter() {
	l.Logger.SetFormatter(&logrus.JSONFormatter{})
}