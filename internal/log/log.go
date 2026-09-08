package log

import (
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var (
	logger *zap.Logger
	level  zapcore.Level = zapcore.DebugLevel
)

func init() {
	cfg := zap.NewDevelopmentConfig()
	cfg.EncoderConfig.TimeKey = "time"
	cfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	cfg.EncoderConfig.StacktraceKey = ""
	logger, _ = cfg.Build()
}

// SetLevel 设置日志级别
func SetLevel(lvl string) {
	switch lvl {
	case "debug":
		level = zapcore.DebugLevel
	case "info":
		level = zapcore.InfoLevel
	case "warn":
		level = zapcore.WarnLevel
	case "error":
		level = zapcore.ErrorLevel
	default:
		level = zapcore.DebugLevel
	}
}

// shouldLog 检查是否应该输出该级别
func shouldLog(msgLevel zapcore.Level) bool {
	return msgLevel >= level
}

// Debug 输出调试日志
func Debug(format string, args ...any) {
	if !shouldLog(zapcore.DebugLevel) {
		return
	}
	logger.Sugar().Debugf(format, args...)
}

// Info 输出信息日志
func Info(format string, args ...any) {
	if !shouldLog(zapcore.InfoLevel) {
		return
	}
	logger.Sugar().Infof(format, args...)
}

// Warn 输出警告日志
func Warn(format string, args ...any) {
	if !shouldLog(zapcore.WarnLevel) {
		return
	}
	logger.Sugar().Warnf(format, args...)
}

// Error 输出错误日志
func Error(format string, args ...any) {
	if !shouldLog(zapcore.ErrorLevel) {
		return
	}
	logger.Sugar().Errorf(format, args...)
}
