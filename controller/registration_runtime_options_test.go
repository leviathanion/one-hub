package controller

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"one-api/common/config"
)

func TestWeChatIdentityRequestReadsCurrentRuntimeOptions(t *testing.T) {
	newServer := func(token, identity string) *httptest.Server {
		t.Helper()
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got := r.Header.Get("Authorization"); got != token {
				http.Error(w, fmt.Sprintf("unexpected token %q", got), http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"success":true,"data":%q}`, identity)
		}))
	}
	firstServer := newServer("token-1", "wechat-1")
	defer firstServer.Close()
	secondServer := newServer("token-2", "wechat-2")
	defer secondServer.Close()

	originalManager := config.GlobalOption
	t.Cleanup(func() { config.GlobalOption = originalManager })
	manager := config.NewOptionManager()
	serverAddress := ""
	serverToken := ""
	manager.RegisterString("WeChatServerAddress", &serverAddress)
	manager.RegisterString("WeChatServerToken", &serverToken)
	config.GlobalOption = manager

	if _, err := manager.PublishRuntimeOverrides(1, map[string]string{
		"WeChatServerAddress": firstServer.URL,
		"WeChatServerToken":   "token-1",
	}); err != nil {
		t.Fatalf("发布初始微信配置：%v", err)
	}
	identity, err := getWeChatIdByCode("code")
	if err != nil || identity != "wechat-1" {
		t.Fatalf("初始微信身份请求失败：identity=%q err=%v", identity, err)
	}

	if _, err := manager.PublishRuntimeOverrides(2, map[string]string{
		"WeChatServerAddress": secondServer.URL,
		"WeChatServerToken":   "token-2",
	}); err != nil {
		t.Fatalf("发布更新后的微信配置：%v", err)
	}
	identity, err = getWeChatIdByCode("code")
	if err != nil || identity != "wechat-2" {
		t.Fatalf("微信身份请求未读取当前配置：identity=%q err=%v", identity, err)
	}
}
