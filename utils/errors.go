package utils

import (
	"errors"
	"fmt"
)

var (
	// Goroutine Pool
	ErrPoolNotStarted = errors.New("协程池未启动")
	ErrPoolStopped = errors.New("协程池已停止")
	ErrPoolFull       = errors.New("协程池队列已满")
    ErrSubmitTimeout  = errors.New("任务提交超时")

	// RustPBX
	ErrASRInitFailed  = errors.New("ASR引擎初始化失败")
    ErrTTSInitFailed  = errors.New("TTS引擎初始化失败")
    ErrLLMInitFailed  = errors.New("LLM引擎初始化失败")

	// Audio
	ErrAudioDecoding  = errors.New("音频解码失败")
    ErrAudioTooLong   = errors.New("音频时长超出限制")
    ErrAudioEmpty     = errors.New("音频数据为空")
)

type RustPBXError struct {
	Service	string
	Op		string
	Err		error
}

func (e *RustPBXError) Error() string {
	return fmt.Sprintf("RustPBX %s %s: %v", e.Service, e.Op, e.Err)
}

func (e *RustPBXError) Unwrap() error {
    return e.Err
}

