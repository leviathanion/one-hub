package codex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

// ParsedAuthFile contains normalized credential data extracted from a Codex auth file.
type ParsedAuthFile struct {
	Credentials *OAuth2Credentials
	Email       string
	FileName    string
}

// CredentialsJSON serializes normalized credentials for storage in one-hub channels.
func (p *ParsedAuthFile) CredentialsJSON() (string, error) {
	if p == nil || p.Credentials == nil {
		return "", fmt.Errorf("credentials are empty")
	}
	return p.Credentials.ToJSON()
}

// DisplayLabel returns the preferred human-readable label for the auth file.
func (p *ParsedAuthFile) DisplayLabel() string {
	if p == nil {
		return ""
	}
	if email := strings.TrimSpace(p.Email); email != "" {
		return email
	}
	fileName := strings.TrimSpace(p.FileName)
	if fileName == "" {
		return ""
	}
	baseName := strings.TrimSpace(strings.TrimSuffix(filepath.Base(fileName), filepath.Ext(fileName)))
	return baseName
}

// ParseAuthFile extracts Codex OAuth credentials from a Codex auth JSON file.
func ParseAuthFile(data []byte, fileName string) (*ParsedAuthFile, error) {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("auth file is empty")
	}

	var raw map[string]any
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.UseNumber()
	if err := decoder.Decode(&raw); err != nil {
		return nil, fmt.Errorf("invalid auth file json: %w", err)
	}

	if provider, ok := raw["type"].(string); ok {
		provider = strings.TrimSpace(provider)
		if provider != "" && !strings.EqualFold(provider, "codex") {
			return nil, fmt.Errorf("unsupported auth file type %q", provider)
		}
	}

	creds, err := FromJSON(string(trimmed))
	if err != nil {
		return nil, fmt.Errorf("invalid codex credentials: %w", err)
	}
	normalizeCredentials(creds)

	if creds.AccessToken == "" {
		return nil, fmt.Errorf("auth file is missing access_token")
	}

	if creds.AccountID == "" {
		if idToken, ok := raw["id_token"].(string); ok {
			if accountID := extractAccountIDFromJWT(strings.TrimSpace(idToken)); accountID != "" {
				creds.AccountID = accountID
			}
		}
	}

	email := ""
	if rawEmail, ok := raw["email"].(string); ok {
		email = strings.TrimSpace(rawEmail)
	}

	return &ParsedAuthFile{
		Credentials: creds,
		Email:       email,
		FileName:    strings.TrimSpace(fileName),
	}, nil
}
