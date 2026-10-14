package antigravity

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"one-api/common/config"
	"one-api/common/credentials"
	"one-api/common/jsonobject"
	"one-api/model"
)

type Credentials struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expires_at"`
	Subject      string `json:"subject"`
	ProjectID    string `json:"project_id"`
	Email        string `json:"email,omitempty"`
}

func ParseCredentials(key string) (*Credentials, error) {
	if len(key) > oauthBodyLimit {
		return nil, errors.New("Antigravity 凭据超过大小限制")
	}
	if _, err := jsonobject.Parse([]byte(key)); err != nil {
		return nil, errors.New("Antigravity 凭据必须是字段无歧义的完整 JSON 对象")
	}
	var result Credentials
	if json.Unmarshal([]byte(key), &result) != nil {
		return nil, errors.New("Antigravity 凭据必须是完整 JSON 对象")
	}
	if err := result.Validate(); err != nil {
		return nil, err
	}
	return &result, nil
}

func (c *Credentials) Validate() error {
	if c == nil || strings.TrimSpace(c.AccessToken) == "" || strings.TrimSpace(c.RefreshToken) == "" || c.ExpiresAt <= 0 || strings.TrimSpace(c.Subject) == "" || strings.TrimSpace(c.ProjectID) == "" {
		return errors.New("Antigravity 凭据缺少 access_token、refresh_token、expires_at、subject 或 project_id，请完成授权")
	}
	if strings.ContainsAny(c.AccessToken, "\r\n") {
		return errors.New("Antigravity access token 格式无效")
	}
	return nil
}

func ValidateCredentialKey(key string) error { _, err := ParseCredentials(key); return err }

func (c *Credentials) ToJSON() (string, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	data, err := json.Marshal(c)
	if err != nil {
		return "", errors.New("无法序列化 Antigravity 凭据")
	}
	if len(data) > oauthBodyLimit {
		return "", errors.New("Antigravity 凭据超过大小限制")
	}
	return string(data), nil
}

var (
	CredentialRotation    = credentials.Service{Store: model.ChannelCredentialStore{}}
	loadCredentialChannel = func(ctx context.Context, id int) (*model.Channel, error) {
		if model.DB == nil {
			return nil, errors.New("database is not initialized")
		}
		channel, err := model.GetChannelByIdWithContext(ctx, id)
		if err == nil {
			channel.SetProxy()
		}
		return channel, err
	}
	newCredentialOAuthClient       = NewOAuthClient
	ErrCredentialRefreshUnresolved = errors.New("Antigravity 凭据刷新结果未确定，请重新授权恢复")
	ErrCredentialRefreshSuperseded = errors.New("Antigravity 渠道凭据已被修改，请重新读取")
)

// ResolveCredentials 不缓存 token，也不初始化 project。只有管理授权允许 onboarding。
// 有 ID 时从数据库读最新连接快照；草稿不能借用已有渠道 ID 刷新或写回。
func ResolveCredentials(ctx context.Context, channel *model.Channel) (*model.Channel, *Credentials, error) {
	if channel == nil || channel.Type != config.ChannelTypeAntigravity {
		return nil, nil, errors.New("Antigravity 渠道配置无效")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if channel.Id <= 0 {
		parsed, err := ParseCredentials(channel.Key)
		if err != nil {
			return nil, nil, err
		}
		if parsed.ExpiresAt <= time.Now().Unix() {
			return nil, nil, errors.New("草稿 access token 已过期，请保存渠道后刷新或重新授权")
		}
		return channel, parsed, nil
	}
	release, err := credentials.AcquireChannelRefreshLock(ctx, channel.Id)
	if err != nil {
		return nil, nil, err
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	latest, err := loadCredentialChannel(ctx, channel.Id)
	if err != nil {
		return nil, nil, err
	}
	if latest == nil || latest.DeletedAt.Valid || latest.Type != config.ChannelTypeAntigravity {
		return nil, nil, ErrCredentialRefreshSuperseded
	}
	parsed, err := ParseCredentials(latest.Key)
	if err != nil {
		return nil, nil, err
	}
	fence, err := credentials.ReadRefresh(latest.InternalState)
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	if fence != nil {
		if parsed.ExpiresAt > now.Unix() {
			return latest, parsed, nil
		}
		return nil, nil, ErrCredentialRefreshUnresolved
	}
	if parsed.ExpiresAt > now.Add(5*time.Minute).Unix() {
		return latest, parsed, nil
	}
	proxy := ""
	if latest.Proxy != nil {
		proxy = *latest.Proxy
	}
	client, err := newCredentialOAuthClient(proxy)
	if err != nil {
		return nil, nil, err
	}
	ticket := credentials.Ticket{ChannelID: latest.Id, Type: latest.Type, ExpectedVersion: latest.Version, AttemptID: uuid.NewString()}
	outcome, err := CredentialRotation.Claim(ctx, ticket, now)
	if err != nil {
		return nil, nil, err
	}
	if outcome != credentials.ClaimAcquired {
		return nil, nil, ErrCredentialRefreshSuperseded
	}
	rotated, err := client.Refresh(ctx, parsed)
	if err != nil {
		if errors.Is(err, ErrOAuthRefreshNotDispatched) {
			cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_, cancelErr := CredentialRotation.Cancel(cancelCtx, ticket)
			return nil, nil, errors.Join(err, cancelErr)
		}
		return nil, nil, errors.Join(ErrCredentialRefreshUnresolved, err)
	}
	key, err := rotated.ToJSON()
	if err != nil {
		return nil, nil, errors.Join(ErrCredentialRefreshUnresolved, err)
	}
	commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	committed, err := CredentialRotation.Commit(commitCtx, ticket, key)
	if committed != credentials.CommitApplied && committed != credentials.CommitAlreadyApplied {
		if committed == credentials.CommitSuperseded {
			return nil, nil, errors.Join(ErrCredentialRefreshSuperseded, err)
		}
		return nil, nil, errors.Join(ErrCredentialRefreshUnresolved, err)
	}
	// Commit 后重新读取，避免返回已被管理员后续保存替换的连接快照。
	latest, err = loadCredentialChannel(commitCtx, channel.Id)
	if err != nil {
		return nil, nil, err
	}
	if latest == nil || latest.DeletedAt.Valid || latest.Type != ticket.Type || latest.Key != key {
		return nil, nil, ErrCredentialRefreshSuperseded
	}
	return latest, rotated, nil
}

// RecoverCredentials 只接受同一 subject/project 的独立授权结果和服务端保存的精确快照。
func RecoverCredentials(ctx context.Context, expected credentials.Snapshot, attemptID string, replacement *Credentials) error {
	if expected.Type != config.ChannelTypeAntigravity || expected.Deleted {
		return credentials.ErrConflict
	}
	previous, err := ParseCredentials(expected.Key)
	if err != nil {
		return credentials.ErrConflict
	}
	if replacement == nil || previous.Subject != replacement.Subject || previous.ProjectID != replacement.ProjectID {
		return credentials.ErrConflict
	}
	key, err := replacement.ToJSON()
	if err != nil {
		return err
	}
	return CredentialRotation.Recover(ctx, expected, attemptID, key)
}
