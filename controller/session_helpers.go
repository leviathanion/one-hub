package controller

import (
	"github.com/gin-contrib/sessions"
	"github.com/gin-gonic/gin"
)

func currentSessionUserID(c *gin.Context) (int, bool) {
	session := sessions.Default(c)
	id, ok := session.Get("id").(int)
	return id, ok
}
