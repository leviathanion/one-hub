package responsesws

import "one-api/common"

func RedactSensitiveText(message string) string {
	return common.RedactSensitiveText(message)
}
