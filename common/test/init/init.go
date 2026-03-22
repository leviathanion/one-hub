package init

import (
	"one-api/common/requester"
	"testing"
)

func init() {
	testing.Init()
	requester.InitHttpClient()
}
