package model

import (
	"encoding/json"
	"strings"

	"github.com/golang-jwt/jwt/v5"
)

// CodexCredentialAccountID 解析持久化凭据的账号身份，兼容 JSON 凭据和原始 JWT。
func CodexCredentialAccountID(key string) string {
	var credential struct {
		AccountID   string `json:"account_id"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(strings.NewReader(key)).Decode(&credential); err != nil {
		return CodexTokenAccountID(key)
	}
	if accountID := strings.TrimSpace(credential.AccountID); accountID != "" {
		return accountID
	}
	return CodexTokenAccountID(credential.AccessToken)
}

// CodexTokenAccountID 只提取账号，不验证 JWT；调用方必须已信任其来源，
// 如原有渠道凭据或通过固定 OAuth 服务端交换取得的结果。
func CodexTokenAccountID(value string) string {
	token, _, err := jwt.NewParser(jwt.WithoutClaimsValidation()).ParseUnverified(value, jwt.MapClaims{})
	if err != nil {
		return ""
	}
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return ""
	}
	auth, ok := claims["https://api.openai.com/auth"].(map[string]any)
	if !ok {
		return ""
	}
	accountID, _ := auth["chatgpt_account_id"].(string)
	return strings.TrimSpace(accountID)
}
