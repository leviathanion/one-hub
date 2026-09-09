// Author: Calcium-Ion
// GitHub: https://github.com/Calcium-Ion/new-api
// Path: service/midjourney.go
package midjourney

import (
	mjProvider "one-api/providers/midjourney"
	"strconv"
	"strings"
)

func CoverActionToModelName(mjAction, model string) string {
	if model == "fast" {
		model = ""
	}

	if model != "" {
		model = model + "_"
	}

	modelName := "mj_" + model + strings.ToLower(mjAction)
	return modelName
}

func GetMjRequestModel(relayMode int, midjRequest *mjProvider.MidjourneyRequest, mjModel string) (string, *mjProvider.MidjourneyResponse, bool) {
	action := ""
	if relayMode == mjProvider.RelayModeMidjourneyAction {
		// plus request
		err := CoverPlusActionToNormalAction(midjRequest)
		if err != nil {
			return "", err, false
		}
		action = midjRequest.Action
	} else {
		switch relayMode {
		case mjProvider.RelayModeMidjourneyImagine:
			action = mjProvider.MjActionImagine
		case mjProvider.RelayModeMidjourneyDescribe:
			action = mjProvider.MjActionDescribe
		case mjProvider.RelayModeMidjourneyBlend:
			action = mjProvider.MjActionBlend
		case mjProvider.RelayModeMidjourneyShorten:
			action = mjProvider.MjActionShorten
		case mjProvider.RelayModeMidjourneyChange:
			action = midjRequest.Action
		case mjProvider.RelayModeMidjourneyModal:
			action = mjProvider.MjActionModal
		case mjProvider.RelayModeMidjourneySwapFace:
			action = mjProvider.MjActionSwapFace
		case mjProvider.RelayModeMidjourneyUpload:
			action = mjProvider.MjActionUpload
		case mjProvider.RelayModeMidjourneySimpleChange:
			if midjRequest == nil || midjRequest.Action == "" {
				return "", mjProvider.MidjourneyErrorWrapper(mjProvider.MjRequestError, "invalid_request"), false
			}
			action = midjRequest.Action
		case mjProvider.RelayModeMidjourneyTaskFetch, mjProvider.RelayModeMidjourneyTaskFetchByCondition:
			return "", nil, true
		default:
			return "", mjProvider.MidjourneyErrorWrapper(mjProvider.MjRequestError, "unknown_relay_action"), false
		}
	}

	modelName := CoverActionToModelName(action, mjModel)
	return modelName, nil, true
}

func normalizeMidjourneySubmitRequest(relayMode int, request *mjProvider.MidjourneyRequest) *mjProvider.MidjourneyResponse {
	if relayMode != mjProvider.RelayModeMidjourneySimpleChange {
		return nil
	}
	if request == nil || strings.TrimSpace(request.Content) == "" {
		return mjProvider.MidjourneyErrorWrapper(mjProvider.MjRequestError, "content_is_required")
	}
	params := ConvertSimpleChangeParams(request.Content)
	if params == nil {
		return mjProvider.MidjourneyErrorWrapper(mjProvider.MjRequestError, "content_parse_failed")
	}
	request.TaskId = params.TaskId
	request.Action = params.Action
	request.Index = params.Index
	return nil
}

func midjourneySubmitUsesOriginTask(relayMode int) bool {
	switch relayMode {
	case mjProvider.RelayModeMidjourneyChange,
		mjProvider.RelayModeMidjourneySimpleChange,
		mjProvider.RelayModeMidjourneyModal:
		return true
	default:
		return false
	}
}

func midjourneyOriginTaskID(relayMode int, request *mjProvider.MidjourneyRequest) (string, *mjProvider.MidjourneyResponse) {
	if !midjourneySubmitUsesOriginTask(relayMode) {
		return "", nil
	}
	if request == nil || strings.TrimSpace(request.TaskId) == "" {
		return "", mjProvider.MidjourneyErrorWrapper(mjProvider.MjRequestError, "task_id_is_required")
	}
	switch relayMode {
	case mjProvider.RelayModeMidjourneyChange:
		if request.Action == "" {
			return "", mjProvider.MidjourneyErrorWrapper(mjProvider.MjRequestError, "action_is_required")
		}
		if request.Index == 0 {
			return "", mjProvider.MidjourneyErrorWrapper(mjProvider.MjRequestError, "index_is_required")
		}
	case mjProvider.RelayModeMidjourneyModal:
		request.Action = mjProvider.MjActionModal
	}
	return strings.TrimSpace(request.TaskId), nil
}

func CoverPlusActionToNormalAction(midjRequest *mjProvider.MidjourneyRequest) *mjProvider.MidjourneyResponse {
	// "customId": "MJ::JOB::upsample::2::3dbbd469-36af-4a0f-8f02-df6c579e7011"
	if midjRequest == nil {
		return mjProvider.MidjourneyErrorWrapper(mjProvider.MjRequestError, "invalid_request")
	}
	customId := midjRequest.CustomId
	if customId == "" {
		return mjProvider.MidjourneyErrorWrapper(mjProvider.MjRequestError, "custom_id_is_required")
	}
	splits := strings.Split(customId, "::")
	if len(splits) < 2 {
		return mjProvider.MidjourneyErrorWrapper(mjProvider.MjRequestError, "custom_id_parse_failed")
	}
	var action string
	if splits[1] == "JOB" {
		if len(splits) < 3 {
			return mjProvider.MidjourneyErrorWrapper(mjProvider.MjRequestError, "custom_id_parse_failed")
		}
		action = splits[2]
	} else {
		action = splits[1]
	}

	if action == "" {
		return mjProvider.MidjourneyErrorWrapper(mjProvider.MjRequestError, "unknown_action")
	}
	if strings.Contains(action, "upsample") {
		if len(splits) < 4 {
			return mjProvider.MidjourneyErrorWrapper(mjProvider.MjRequestError, "custom_id_parse_failed")
		}
		index, err := strconv.Atoi(splits[3])
		if err != nil {
			return mjProvider.MidjourneyErrorWrapper(mjProvider.MjRequestError, "index_parse_failed")
		}
		midjRequest.Index = index
		midjRequest.Action = mjProvider.MjActionUpscale
	} else if strings.Contains(action, "variation") {
		midjRequest.Index = 1
		if action == "variation" {
			if len(splits) < 4 {
				return mjProvider.MidjourneyErrorWrapper(mjProvider.MjRequestError, "custom_id_parse_failed")
			}
			index, err := strconv.Atoi(splits[3])
			if err != nil {
				return mjProvider.MidjourneyErrorWrapper(mjProvider.MjRequestError, "index_parse_failed")
			}
			midjRequest.Index = index
			midjRequest.Action = mjProvider.MjActionVariation
		} else if action == "low_variation" {
			midjRequest.Action = mjProvider.MjActionLowVariation
		} else if action == "high_variation" {
			midjRequest.Action = mjProvider.MjActionHighVariation
		}
	} else if strings.Contains(action, "pan") {
		midjRequest.Action = mjProvider.MjActionPan
		midjRequest.Index = 1
	} else if strings.Contains(action, "reroll") {
		midjRequest.Action = mjProvider.MjActionReRoll
		midjRequest.Index = 1
	} else if action == "Outpaint" {
		midjRequest.Action = mjProvider.MjActionZoom
		midjRequest.Index = 1
	} else if action == "CustomZoom" {
		midjRequest.Action = mjProvider.MjActionCustomZoom
		midjRequest.Index = 1
	} else if action == "Inpaint" {
		midjRequest.Action = mjProvider.MjActionInPaint
		midjRequest.Index = 1
	} else {
		return mjProvider.MidjourneyErrorWrapper(mjProvider.MjRequestError, "unknown_action:"+customId)
	}
	return nil
}

func ConvertSimpleChangeParams(content string) *mjProvider.MidjourneyRequest {
	split := strings.Fields(content)
	if len(split) != 2 {
		return nil
	}

	action := strings.ToLower(split[1])
	changeParams := &mjProvider.MidjourneyRequest{}
	changeParams.TaskId = split[0]

	if action == "r" {
		changeParams.Action = mjProvider.MjActionReRoll
		return changeParams
	}
	if len(action) != 2 {
		return nil
	}
	if action[0] == 'u' {
		changeParams.Action = mjProvider.MjActionUpscale
	} else if action[0] == 'v' {
		changeParams.Action = mjProvider.MjActionVariation
	} else {
		return nil
	}

	index, err := strconv.Atoi(action[1:2])
	if err != nil || index < 1 || index > 4 {
		return nil
	}
	changeParams.Index = index
	return changeParams
}
