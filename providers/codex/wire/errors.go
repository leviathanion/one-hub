package wire

import "fmt"

type Violation struct {
	Param   string
	Message string
}

func (v *Violation) Error() string {
	if v == nil {
		return ""
	}
	if v.Param == "" {
		return v.Message
	}
	return fmt.Sprintf("%s: %s", v.Param, v.Message)
}

func reject(param, format string, args ...any) *Violation {
	return &Violation{Param: param, Message: fmt.Sprintf(format, args...)}
}
