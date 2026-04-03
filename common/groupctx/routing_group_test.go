package groupctx

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"one-api/common/config"

	"github.com/gin-gonic/gin"
)

func newRoutingGroupTestContext() *gin.Context {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	return ctx
}

func TestCurrentRoutingGroupFallbacksAndSource(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx := newRoutingGroupTestContext()
	ctx.Set("token_group", "token-a")
	ctx.Set("group", "user-a")
	ctx.Set("token_backup_group", "backup-a")

	if got := CurrentRoutingGroup(ctx); got != "token-a" {
		t.Fatalf("expected token group fallback, got %q", got)
	}
	if got := CurrentRoutingGroupSource(ctx); got != RoutingGroupSourceTokenGroup {
		t.Fatalf("expected token group source, got %q", got)
	}

	SetRoutingGroup(ctx, "backup-a", RoutingGroupSourceBackupGroup)
	if got := ctx.GetString(config.GinRoutingGroupKey); got != "backup-a" {
		t.Fatalf("expected explicit routing group to be stored, got %q", got)
	}
	if got := CurrentRoutingGroup(ctx); got != "backup-a" {
		t.Fatalf("expected explicit routing group to win, got %q", got)
	}
	if got := CurrentRoutingGroupSource(ctx); got != RoutingGroupSourceBackupGroup {
		t.Fatalf("expected explicit routing group source, got %q", got)
	}
}

func TestCurrentRoutingGroupMeta(t *testing.T) {
	gin.SetMode(gin.TestMode)

	ctx := newRoutingGroupTestContext()
	ctx.Set("token_group", "token-a")
	ctx.Set("token_backup_group", "backup-a")
	ctx.Set("is_backupGroup", true)
	SetRoutingGroup(ctx, "backup-a", RoutingGroupSourceBackupGroup)

	meta := CurrentRoutingGroupMeta(ctx)
	if meta["using_group"] != "backup-a" || meta["token_group"] != "token-a" {
		t.Fatalf("expected routing group metadata to expose using/token group, got %#v", meta)
	}
	if meta["routing_group_source"] != RoutingGroupSourceBackupGroup || meta["is_backup_group"] != true {
		t.Fatalf("expected routing group metadata to expose source and backup flag, got %#v", meta)
	}
}
