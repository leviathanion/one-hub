package wire

import "fmt"

// CapabilityError 表示在发送前发现的协议不可表示，不是上游参数校验错误。
type CapabilityError struct {
	Param   string
	Message string
}

func (e *CapabilityError) Error() string { return e.Param + ": " + e.Message }

func unsupported(path, message string) error {
	return &CapabilityError{Param: path, Message: message}
}

func invalid(path string, err error) error {
	return unsupported(path, fmt.Sprintf("无法转换请求结构：%v", err))
}
