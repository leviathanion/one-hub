package telegram

import (
	"context"
	"encoding/json"
	"github.com/PaulSonOfLars/gotgbot/v2"
	"github.com/PaulSonOfLars/gotgbot/v2/ext"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"one-api/common/config"
	"one-api/internal/testutil/sqlitetest"
	"one-api/model"
	"strings"
	"testing"
)

type reviewBotClient struct {
	gotgbot.BaseBotClient
	sent []map[string]string
}

func (c *reviewBotClient) RequestWithContext(ctx context.Context, token, method string, params map[string]string, data map[string]gotgbot.FileReader, opts *gotgbot.RequestOpts) (json.RawMessage, error) {
	copied := map[string]string{}
	for k, v := range params {
		copied[k] = v
	}
	copied["method"] = method
	c.sent = append(c.sent, copied)
	if method == "answerCallbackQuery" {
		return json.RawMessage(`true`), nil
	}
	return json.RawMessage(`{"message_id":2,"date":1,"chat":{"id":42,"type":"private"}}`), nil
}
func TestUserSecurityApikeyOutputBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, chatType string
		status         int
		wantKey        bool
	}{{"private_enabled", "private", 1, true}, {"group_enabled", "supergroup", 1, false}, {"normal_group", "group", 1, false}, {"private_disabled", "private", 2, false}, {"private_deleted", "private", 1, false}} {
		t.Run(tc.name, func(t *testing.T) {
			db, err := gorm.Open(sqlite.Open(sqlitetest.MemoryDSN()), &gorm.Config{})
			if err != nil {
				t.Fatal(err)
			}
			if err := db.AutoMigrate(&model.User{}, &model.Token{}); err != nil {
				t.Fatal(err)
			}
			oldDB, oldOptions := model.DB, config.GlobalOption
			model.DB = db
			manager := config.NewOptionManager()
			address := ""
			manager.RegisterString("ServerAddress", &address)
			config.GlobalOption = manager
			if _, err := manager.PublishRuntimeOverrides(1, map[string]string{"ServerAddress": ""}); err != nil {
				t.Fatal(err)
			}
			sqlDB, _ := db.DB()
			t.Cleanup(func() { model.DB, config.GlobalOption = oldDB, oldOptions; _ = sqlDB.Close() })
			if err := db.Create(&model.User{Id: 1, Username: "bot-user", Password: "fixture-password", TelegramId: 42, Status: tc.status}).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Session(&gorm.Session{SkipHooks: true}).Create(&model.Token{UserId: 1, Key: "review-api-key", Name: "token"}).Error; err != nil {
				t.Fatal(err)
			}
			if tc.name == "private_deleted" {
				if err := db.Delete(&model.User{}, 1).Error; err != nil {
					t.Fatal(err)
				}
			}
			client := &reviewBotClient{}
			bot, err := gotgbot.NewBot("123:fixture", &gotgbot.BotOpts{DisableTokenCheck: true, BotClient: client})
			if err != nil {
				t.Fatal(err)
			}
			chatID := int64(42)
			if tc.chatType != "private" {
				chatID = -10042
			}
			message := &gotgbot.Message{MessageId: 1, From: &gotgbot.User{Id: 42}, Chat: gotgbot.Chat{Id: chatID, Type: tc.chatType}, Text: "/apikey"}
			ctx := ext.NewContext(bot, &gotgbot.Update{Message: message}, nil)
			if err := commandApikeyStart(bot, ctx); err != nil {
				t.Fatal(err)
			}
			found := false
			destination := ""
			for _, request := range client.sent {
				if strings.Contains(request["text"], "sk-review-api-key") {
					found = true
					destination = request["chat_id"]
				}
			}
			if found != tc.wantKey {
				t.Fatalf("API key 输出越界: chat=%s status=%d key_sent=%v destination=%s", tc.chatType, tc.status, found, destination)
			}
			// 分页来自独立更新，必须重新检查聊天类型和当前账户。
			client.sent = nil
			callback := &gotgbot.CallbackQuery{Id: "page", From: gotgbot.User{Id: 42}, Message: message, Data: "p:apikey,1"}
			pageCtx := ext.NewContext(bot, &gotgbot.Update{CallbackQuery: callback}, nil)
			if err := paginationHandler(bot, pageCtx); err != nil {
				t.Fatal(err)
			}
			found = false
			for _, request := range client.sent {
				if strings.Contains(request["text"], "sk-review-api-key") {
					found = true
				}
			}
			if found != tc.wantKey {
				t.Fatalf("分页密钥输出越界: chat=%s status=%d found=%v", tc.chatType, tc.status, found)
			}
		})
	}
}
