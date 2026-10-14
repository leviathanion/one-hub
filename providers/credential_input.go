package providers

import (
	"one-api/model"
	"strings"
)

// 凭据的整体/按行输入由 factory 声明，管理入口不猜测 JSON 或复制渠道类型分支。
type wholeCredentialInputFactory interface {
	WholeCredentialInput() bool
}

type credentialInputPreparer interface {
	PrepareCredentialInput(string) (string, error)
}

func ChannelCredentialInputs(channel *model.Channel) []string {
	if channel == nil {
		return nil
	}
	if UsesWholeCredentialInput(channel) {
		return []string{channel.Key}
	}
	return strings.Split(channel.Key, "\n")
}

func UsesWholeCredentialInput(channel *model.Channel) bool {
	factory, ok := providerFactoryForChannel(channel).(wholeCredentialInputFactory)
	return ok && factory.WholeCredentialInput()
}

// PrepareChannelCredentialInput 在所有管理写入入口校验并规范化凭据，不进行网络刷新。
func PrepareChannelCredentialInput(channel *model.Channel) error {
	if channel == nil {
		return nil
	}
	if factory, ok := providerFactoryForChannel(channel).(credentialInputPreparer); ok {
		key, err := factory.PrepareCredentialInput(channel.Key)
		if err != nil {
			return err
		}
		channel.Key = key
	}
	return nil
}
