package webauthn

import (
	"log"
	"net/url"
	"one-api/common/config"
	"strings"

	"github.com/go-webauthn/webauthn/webauthn"
)

// InitWebAuthn 验证当前 WebAuthn 配置。
func InitWebAuthn() error {
	_, err := newWebAuthn()
	return err
}

func newWebAuthn() (*webauthn.WebAuthn, error) {
	options := config.GlobalOption.RuntimeSnapshot()
	serverAddress := options.String("ServerAddress", config.ServerAddress)
	rpID := extractRPID(serverAddress)
	instance, err := webauthn.New(&webauthn.Config{
		RPDisplayName: options.String("SystemName", config.SystemName), // 显示名称
		RPID:          rpID,                                            // FQDN
		RPOrigins: []string{
			serverAddress,
		}, // 添加允许的源地址列表
	})
	if err != nil {
		return nil, err
	}
	return instance, nil
}

// GetWebAuthn 按当前配置为本次 ceremony 创建 WebAuthn 实例。
func GetWebAuthn() (*webauthn.WebAuthn, error) {
	return newWebAuthn()
}

// extractRPID 从服务器地址提取有效的 RPID
func extractRPID(serverAddress string) string {
	// 如果是 localhost，直接返回 localhost
	if strings.Contains(serverAddress, "localhost") {
		return "localhost"
	}

	// 解析URL获取主机名
	if !strings.HasPrefix(serverAddress, "http://") && !strings.HasPrefix(serverAddress, "https://") {
		serverAddress = "http://" + serverAddress
	}

	u, err := url.Parse(serverAddress)
	if err != nil {
		log.Printf("解析服务器地址失败: %v, 使用默认 localhost", err)
		return "localhost"
	}

	// 移除端口号，只保留主机名
	host := u.Hostname()
	if host == "" {
		return "localhost"
	}

	return host
}
