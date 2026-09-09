// Author: Calcium-Ion
// GitHub: https://github.com/Calcium-Ion/new-api
// Path: relay/relay-mj.go
package midjourney

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"one-api/common"
	"one-api/middleware"
	"one-api/model"
	"one-api/providers"
	provider "one-api/providers/midjourney"
	"one-api/relay"
	"one-api/relay/relay_util"
	taskprogress "one-api/relay/task"
	taskbase "one-api/relay/task/base"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func coverMidjourneyTaskDto(originTask *model.Midjourney) (midjourneyTask provider.MidjourneyDto) {
	midjourneyTask.MjId = originTask.MjId
	midjourneyTask.Progress = originTask.Progress
	midjourneyTask.PromptEn = originTask.PromptEn
	midjourneyTask.State = originTask.State
	midjourneyTask.SubmitTime = originTask.SubmitTime
	midjourneyTask.StartTime = originTask.StartTime
	midjourneyTask.FinishTime = originTask.FinishTime
	midjourneyTask.ImageUrl = ""

	midjourneyTask.ImageUrl = originTask.ImageUrl
	midjourneyTask.Status = originTask.Status
	midjourneyTask.FailReason = originTask.FailReason
	midjourneyTask.Action = originTask.Action
	midjourneyTask.Description = originTask.Description
	midjourneyTask.Prompt = originTask.Prompt
	if originTask.Buttons != "" {
		var buttons []provider.ActionButton
		err := json.Unmarshal([]byte(originTask.Buttons), &buttons)
		if err == nil {
			midjourneyTask.Buttons = buttons
		}
	}
	if originTask.Properties != "" {
		var properties provider.Properties
		err := json.Unmarshal([]byte(originTask.Properties), &properties)
		if err == nil {
			midjourneyTask.Properties = &properties
		}
	}
	return
}

func RelaySwapFace(c *gin.Context) *provider.MidjourneyResponse {
	mjProvider, errWithMJ := getMJProviderWithRequest(c, provider.RelayModeMidjourneySwapFace, nil)
	if errWithMJ != nil {
		return errWithMJ
	}

	startTime := time.Now().UnixNano() / int64(time.Millisecond)
	userId := c.GetInt("id")
	tokenId := c.GetInt("token_id")
	var swapFaceRequest provider.SwapFaceRequest
	err := common.UnmarshalBodyReusable(c, &swapFaceRequest)
	if err != nil {
		return provider.MidjourneyErrorWrapper(provider.MjRequestError, "bind_request_body_failed")
	}
	if swapFaceRequest.SourceBase64 == "" || swapFaceRequest.TargetBase64 == "" {
		return provider.MidjourneyErrorWrapper(provider.MjRequestError, "sour_base64_and_target_base64_is_required")
	}

	requestURL := getMjRequestPath(c.Request.URL.String())
	mjModelType := c.GetString("mj_model")
	midjourneyTask := &model.Midjourney{
		UserId: userId, TokenID: tokenId, Action: provider.MjActionSwapFace,
		Prompt: "InsightFace", SubmitTime: startTime, Progress: "0%",
		ChannelId: c.GetInt("channel_id"), Mode: mjModelType,
	}
	taskOwner, ownerErr := prepareMidjourneyTaskOwner(c, midjourneyTask, true, 0)
	if ownerErr != nil {
		return ownerErr
	}

	mjResp, _, err := mjProvider.Send(60, requestURL)
	if err != nil {
		closeMidjourneyTaskOwner(c, taskOwner, model.TaskStatusUnknown, err.Error())
		if mjResp != nil {
			return &mjResp.Response
		}
		return provider.MidjourneyErrorWrapper(provider.MjRequestError, "midjourney_submit_failed")
	}

	midjResponse := &mjResp.Response
	midjourneyTask.Code = midjResponse.Code
	midjourneyTask.MjId = midjResponse.Result
	midjourneyTask.Description = midjResponse.Description
	midjourneyTask.StartTime = time.Now().UnixNano() / int64(time.Millisecond)
	if midjResponse.Code != 1 && midjResponse.Code != 21 && midjResponse.Code != 22 {
		closeMidjourneyTaskOwner(c, taskOwner, model.TaskStatusFailure, midjResponse.Description)
		return midjResponse
	}
	if acceptErr := acceptMidjourneyTaskOwner(c, taskOwner, midjourneyTask); acceptErr != nil {
		return acceptErr
	}

	c.Writer.WriteHeader(mjResp.StatusCode)
	respBody, err := json.Marshal(midjResponse)
	if err != nil {
		return provider.MidjourneyErrorWrapper(provider.MjRequestError, "unmarshal_response_body_failed")
	}
	_, err = io.Copy(c.Writer, bytes.NewBuffer(respBody))
	if err != nil {
		return provider.MidjourneyErrorWrapper(provider.MjRequestError, "copy_response_body_failed")
	}
	return nil
}

func RelayMidjourneyTaskImageSeed(c *gin.Context) *provider.MidjourneyResponse {
	taskId := c.Param("id")
	userId := c.GetInt("id")
	originTask := model.GetByMJId(userId, taskId)
	if originTask == nil {
		return provider.MidjourneyErrorWrapper(provider.MjRequestError, "task_no_found")
	}

	mjProvider, errWithMJ := getMJProviderWithChannelId(c, originTask.ChannelId)
	if errWithMJ != nil {
		return errWithMJ
	}

	requestURL := getMjRequestPath(c.Request.URL.String())
	midjResponseWithStatus, _, err := mjProvider.Send(30, requestURL)
	if err != nil {
		return &midjResponseWithStatus.Response
	}
	midjResponse := &midjResponseWithStatus.Response
	c.Writer.WriteHeader(midjResponseWithStatus.StatusCode)
	respBody, err := json.Marshal(midjResponse)
	if err != nil {
		return provider.MidjourneyErrorWrapper(provider.MjRequestError, "unmarshal_response_body_failed")
	}
	_, err = io.Copy(c.Writer, bytes.NewBuffer(respBody))
	if err != nil {
		return provider.MidjourneyErrorWrapper(provider.MjRequestError, "copy_response_body_failed")
	}
	return nil
}

func RelayMidjourneyTask(c *gin.Context, relayMode int) *provider.MidjourneyResponse {
	userId := c.GetInt("id")
	var err error
	var respBody []byte
	switch relayMode {
	case provider.RelayModeMidjourneyTaskFetch:
		taskId := c.Param("id")
		originTask := model.GetByMJId(userId, taskId)
		if originTask == nil {
			return &provider.MidjourneyResponse{
				Code:        4,
				Description: "task_no_found",
			}
		}
		midjourneyTask := coverMidjourneyTaskDto(originTask)
		respBody, err = json.Marshal(midjourneyTask)
		if err != nil {
			return &provider.MidjourneyResponse{
				Code:        4,
				Description: "unmarshal_response_body_failed",
			}
		}
	case provider.RelayModeMidjourneyTaskFetchByCondition:
		var condition = struct {
			IDs []string `json:"ids"`
		}{}
		err = c.BindJSON(&condition)
		if err != nil {
			return &provider.MidjourneyResponse{
				Code:        4,
				Description: "do_request_failed",
			}
		}
		var tasks []provider.MidjourneyDto
		if len(condition.IDs) != 0 {
			originTasks := model.GetByMJIds(userId, condition.IDs)
			for _, originTask := range originTasks {
				midjourneyTask := coverMidjourneyTaskDto(originTask)
				tasks = append(tasks, midjourneyTask)
			}
		}
		if tasks == nil {
			tasks = make([]provider.MidjourneyDto, 0)
		}
		respBody, err = json.Marshal(tasks)
		if err != nil {
			return &provider.MidjourneyResponse{
				Code:        4,
				Description: "unmarshal_response_body_failed",
			}
		}
	}

	c.Writer.Header().Set("Content-Type", "application/json")

	_, err = io.Copy(c.Writer, bytes.NewBuffer(respBody))
	if err != nil {
		return &provider.MidjourneyResponse{
			Code:        4,
			Description: "copy_response_body_failed",
		}
	}
	return nil
}

func RelayMidjourneySubmit(c *gin.Context, relayMode int) *provider.MidjourneyResponse {
	userId := c.GetInt("id")
	tokenId := c.GetInt("token_id")
	consumeQuota := true
	mjModelType := c.GetString("mj_model")
	var midjRequest provider.MidjourneyRequest
	err := common.UnmarshalBodyReusable(c, &midjRequest)
	if err != nil {
		return provider.MidjourneyErrorWrapper(provider.MjRequestError, "bind_request_body_failed")
	}
	if mjErr := normalizeMidjourneySubmitRequest(relayMode, &midjRequest); mjErr != nil {
		return mjErr
	}

	var mjProvider *provider.MidjourneyProvider
	ownerChannelID := 0

	if relayMode == provider.RelayModeMidjourneyAction { // midjourney plus，需要从customId中获取任务信息
		mjErr := CoverPlusActionToNormalAction(&midjRequest)
		if mjErr != nil {
			return mjErr
		}
		relayMode = provider.RelayModeMidjourneyChange
	}

	if relayMode == provider.RelayModeMidjourneyImagine { //绘画任务，此类任务可重复
		if midjRequest.Prompt == "" {
			return provider.MidjourneyErrorWrapper(provider.MjRequestError, "prompt_is_required")
		}
		midjRequest.Action = provider.MjActionImagine
	} else if relayMode == provider.RelayModeMidjourneyDescribe { //按图生文任务，此类任务可重复
		midjRequest.Action = provider.MjActionDescribe
	} else if relayMode == provider.RelayModeMidjourneyShorten { //缩短任务，此类任务可重复，plus only
		if strings.TrimSpace(midjRequest.Prompt) == "" {
			return provider.MidjourneyErrorWrapper(provider.MjRequestError, "prompt_is_required")
		}
		midjRequest.Action = provider.MjActionShorten
	} else if relayMode == provider.RelayModeMidjourneyBlend { //绘画任务，此类任务可重复
		midjRequest.Action = provider.MjActionBlend
	} else if relayMode == provider.RelayModeMidjourneyUpload { //绘画任务，此类任务可重复
		midjRequest.Action = provider.MjActionUpload
	}

	if midjourneySubmitUsesOriginTask(relayMode) { //放大、变换任务绑定原任务渠道
		mjId, originErr := midjourneyOriginTaskID(relayMode, &midjRequest)
		if originErr != nil {
			return originErr
		}

		originTask := model.GetByMJId(userId, mjId)
		if originTask == nil {
			return provider.MidjourneyErrorWrapper(provider.MjRequestError, "task_not_found")
		} else if originTask.Status != "SUCCESS" && relayMode != provider.RelayModeMidjourneyModal {
			return provider.MidjourneyErrorWrapper(provider.MjRequestError, "task_status_not_success")
		} else { //原任务的Status=SUCCESS，则可以做放大UPSCALE、变换VARIATION等动作，此时必须使用原来的请求地址才能正确处理
			if originTask.Mode != "" {
				mjModelType = originTask.Mode
				c.Set("mj_model", mjModelType)
			}
			if apiErr := middleware.RefreshAuthenticatedLongLivedPrincipal(c); apiErr != nil {
				return MidjourneyErrorFromInternal(provider.MjRequestError, "midjourney_work_not_allowed: "+apiErr.Message)
			}
			modelName := CoverActionToModelName(midjRequest.Action, mjModelType)
			if err := middleware.EnsureTokenModelAllowed(c, modelName); err != nil {
				c.AbortWithStatus(http.StatusNotFound)
				return MidjourneyErrorFromInternal(provider.MjErrorUnknown, "无法获取provider:"+err.Error())
			}
			var errWithMJ *provider.MidjourneyResponse
			mjProvider, errWithMJ = getMJProviderWithChannelId(c, originTask.ChannelId)
			if errWithMJ != nil {
				return errWithMJ
			}

			log.Printf("检测到此操作为放大、变换、重绘，获取原channel信息: %d", originTask.ChannelId)
			ownerChannelID = originTask.ChannelId
		}
		midjRequest.Prompt = originTask.Prompt

		//if channelType == common.ChannelTypeMidjourneyPlus {
		//	// plus
		//} else {
		//	// 普通版渠道
		//
		//}
	}
	if mjProvider == nil {
		var errWithMJ *provider.MidjourneyResponse
		mjProvider, errWithMJ = getMJProviderWithRequest(c, relayMode, &midjRequest)
		if errWithMJ != nil {
			return errWithMJ
		}
	}

	if midjRequest.Action == provider.MjActionInPaint || midjRequest.Action == provider.MjActionCustomZoom {
		consumeQuota = false
	}

	//baseURL := common.ChannelBaseURLs[channelType]
	requestURL := getMjRequestPath(c.Request.URL.String())

	midjourneyTask := &model.Midjourney{
		UserId: userId, TokenID: tokenId, Action: midjRequest.Action,
		Prompt: midjRequest.Prompt, SubmitTime: time.Now().UnixNano() / int64(time.Millisecond),
		Progress: "0%", ChannelId: c.GetInt("channel_id"), Mode: mjModelType,
	}
	taskOwner, ownerErr := prepareMidjourneyTaskOwner(c, midjourneyTask, consumeQuota, ownerChannelID)
	if ownerErr != nil {
		return ownerErr
	}

	midjResponseWithStatus, responseBody, err := mjProvider.Send(60, requestURL)
	if err != nil {
		closeMidjourneyTaskOwner(c, taskOwner, model.TaskStatusUnknown, err.Error())
		if midjResponseWithStatus != nil {
			return &midjResponseWithStatus.Response
		}
		return provider.MidjourneyErrorWrapper(provider.MjRequestError, "midjourney_submit_failed")
	}

	midjResponse := &midjResponseWithStatus.Response
	isUpload := relayMode == provider.RelayModeMidjourneyUpload
	var uploadResponse provider.MidjourneyUploadResponse
	uploadOK := false
	if isUpload {
		uploadResponse, uploadOK = parseMidjourneyUploadSuccess(responseBody)
	}

	// 文档：https://github.com/novicezk/midjourney-proxy/blob/main/docs/api.md
	//1-提交成功
	// 21-任务已存在（处理中或者有结果了） {"code":21,"description":"任务已存在","result":"0741798445574458","properties":{"status":"SUCCESS","imageUrl":"https://xxxx"}}
	// 22-排队中 {"code":22,"description":"排队中，前面还有1个任务","result":"0741798445574458","properties":{"numberOfQueues":1,"discordInstanceId":"1118138338562560102"}}
	// 23-队列已满，请稍后再试 {"code":23,"description":"队列已满，请稍后尝试","result":"14001929738841620","properties":{"discordInstanceId":"1118138338562560102"}}
	// 24-prompt包含敏感词 {"code":24,"description":"可能包含敏感词","properties":{"promptEn":"nude body","bannedWord":"nude"}}
	// other: 提交错误，description为错误描述
	midjourneyTask.Code = midjResponse.Code
	midjourneyTask.MjId = midjResponse.Result
	midjourneyTask.Description = midjResponse.Description

	// 上传成功返回 URL 数组，不生成可轮询的 provider task ID。
	if isUpload && uploadOK {
		midjourneyTask.Code = uploadResponse.Code
		midjourneyTask.Description = uploadResponse.Description
		midjourneyTask.Status = string(model.TaskStatusSuccess)
		midjourneyTask.Progress = "100%"
		midjourneyTask.StartTime = time.Now().UnixNano() / int64(time.Millisecond)
		midjourneyTask.FinishTime = midjourneyTask.StartTime
		if syncErr := completeMidjourneyUploadOwner(c, taskOwner, midjourneyTask); syncErr != nil {
			return syncErr
		}
	}
	if isUpload && !uploadOK && len(bytes.TrimSpace(responseBody)) == 0 {
		midjourneyTask.FailReason = "empty_upload_response"
		closeMidjourneyTaskOwner(c, taskOwner, model.TaskStatusFailure, midjourneyTask.FailReason)
		return provider.MidjourneyErrorWrapper(provider.MjRequestError, "midjourney_upload_response_invalid")
	}

	if (midjResponse.Code != 1 && midjResponse.Code != 21 && midjResponse.Code != 22) || (isUpload && !uploadOK) {
		//非1-提交成功,21-任务已存在和22-排队中，则记录错误原因
		midjourneyTask.FailReason = midjResponse.Description
		if isUpload && midjResponse.Code == 1 {
			midjourneyTask.FailReason = "invalid_upload_response"
		}
		closeMidjourneyTaskOwner(c, taskOwner, model.TaskStatusFailure, midjourneyTask.FailReason)
		if isUpload && midjResponse.Code == 1 {
			return provider.MidjourneyErrorWrapper(provider.MjRequestError, "midjourney_upload_response_invalid")
		}
	}

	if !isUpload && midjResponse.Code == 21 { //21-任务已存在（处理中或者有结果了）
		// 将 properties 转换为一个 map
		properties, ok := midjResponse.Properties.(map[string]interface{})
		if ok {
			imageUrl, ok1 := properties["imageUrl"].(string)
			status, ok2 := properties["status"].(string)
			if ok1 && ok2 {
				midjourneyTask.ImageUrl = imageUrl
				midjourneyTask.Status = status
				if status == "SUCCESS" {
					midjourneyTask.Progress = "100%"
					midjourneyTask.StartTime = time.Now().UnixNano() / int64(time.Millisecond)
					midjourneyTask.FinishTime = time.Now().UnixNano() / int64(time.Millisecond)
					midjResponse.Code = 1
				}
			}
		}
		//修改返回值
		if midjRequest.Action != provider.MjActionInPaint && midjRequest.Action != provider.MjActionCustomZoom {
			newBody := strings.Replace(string(responseBody), `"code":21`, `"code":1`, -1)
			responseBody = []byte(newBody)
		}
	}

	if !isUpload && midjResponse.Code == 1 && midjRequest.Action == "UPLOAD" {
		midjourneyTask.Progress = "100%"
		midjourneyTask.Status = "SUCCESS"
	}

	if !isUpload && (midjResponse.Code == 1 || midjResponse.Code == 21 || midjResponse.Code == 22) {
		if acceptErr := acceptMidjourneyTaskOwner(c, taskOwner, midjourneyTask); acceptErr != nil {
			return acceptErr
		}
	}

	if !isUpload && midjResponse.Code == 22 { //22-排队中，说明任务已存在
		//修改返回值
		newBody := strings.Replace(string(responseBody), `"code":22`, `"code":1`, -1)
		responseBody = []byte(newBody)
	}

	//resp.Body = io.NopCloser(bytes.NewBuffer(responseBody))
	bodyReader := io.NopCloser(bytes.NewBuffer(responseBody))

	//for k, v := range resp.Header {
	//	c.Writer.Header().Set(k, v[0])
	//}
	c.Writer.WriteHeader(midjResponseWithStatus.StatusCode)

	_, err = io.Copy(c.Writer, bodyReader)
	if err != nil {
		return &provider.MidjourneyResponse{
			Code:        4,
			Description: "copy_response_body_failed",
		}
	}
	err = bodyReader.Close()
	if err != nil {
		return &provider.MidjourneyResponse{
			Code:        4,
			Description: "close_response_body_failed",
		}
	}
	return nil
}

func getMjRequestPath(path string) string {
	requestURL := path
	if strings.Contains(requestURL, "/mj-") {
		urls := strings.Split(requestURL, "/mj/")
		if len(urls) < 2 {
			return requestURL
		}
		requestURL = "/mj/" + urls[1]
	}
	return requestURL
}

func prepareMidjourneyTaskOwner(c *gin.Context, view *model.Midjourney, chargeable bool, ownerChannelID int) (*model.Task, *provider.MidjourneyResponse) {
	if c == nil || view == nil {
		return nil, provider.MidjourneyErrorWrapper(provider.MjRequestError, "task_owner_required")
	}
	modelName := CoverActionToModelName(view.Action, view.Mode)
	if apiErr := middleware.AdmitAuthenticatedChannelWork(c, modelName, c.GetInt("channel_id")); apiErr != nil {
		return nil, MidjourneyErrorFromInternal(provider.MjRequestError, "midjourney_work_not_allowed: "+apiErr.Message)
	}
	fingerprint := midjourneyTaskRequestFingerprint(c, view)
	task := &model.Task{
		Platform:                     model.TaskPlatformMidjourney,
		UserId:                       c.GetInt("id"),
		TokenID:                      c.GetInt("token_id"),
		ChannelId:                    c.GetInt("channel_id"),
		Action:                       view.Action,
		Status:                       model.TaskStatusSubmitted,
		SubmitTime:                   view.SubmitTime,
		Progress:                     0,
		Data:                         model.EncodeMidjourneyTaskData(view),
		ReservedQuota:                0,
		ProviderNamespace:            "task-platform:midjourney",
		ProviderTaskScopeIncarnation: "provider-wide",
		RequestFingerprint:           fingerprint,
	}
	if ownerChannelID > 0 && task.ChannelId != ownerChannelID {
		return nil, provider.MidjourneyErrorWrapper(provider.MjRequestError, "midjourney_owner_channel_conflict")
	}
	var (
		reserve model.BillingBalanceResult
		err     error
	)
	createOwner := model.CreateTaskBillingOwner
	if ownerChannelID > 0 {
		createOwner = model.CreateTaskBillingOwnerForBoundChannel
	}
	if chargeable {
		quota, priceErr := relay_util.NewPricedQuota(c, modelName, 1)
		if priceErr != nil {
			return nil, provider.MidjourneyErrorWrapper(provider.MjRequestError, "midjourney_price_unavailable")
		}
		reservationQuota, priceErr := quota.ReservationQuota()
		if priceErr != nil {
			return nil, provider.MidjourneyErrorWrapper(provider.MjRequestError, "midjourney_price_unavailable")
		}
		task.ReservedQuota = int64(reservationQuota)
		admissionCtx, cancelAdmission := relay_util.BoundedBillingAdmissionContext(c.Request.Context())
		reserve, err = createOwner(admissionCtx, task)
		cancelAdmission()
	} else {
		admissionCtx, cancelAdmission := relay_util.BoundedBillingAdmissionContext(c.Request.Context())
		reserve, err = createOwner(admissionCtx, task)
		cancelAdmission()
	}
	if err != nil || reserve.Outcome != model.BillingBalanceCommitted {
		return nil, provider.MidjourneyErrorWrapper(provider.MjRequestError, "midjourney_billing_admission_failed")
	}
	claimID := uuid.NewString()
	var claim model.TaskMutationResult
	for attempt := 0; attempt < 2; attempt++ {
		claim, err = model.ClaimTaskSubmission(c.Request.Context(), task, claimID)
		if claim.Outcome != model.TaskMutationDefinitelyNotApplied || err == nil {
			break
		}
	}
	if err != nil || claim.Outcome != model.TaskMutationApplied {
		if claim.Outcome == model.TaskMutationDefinitelyNotApplied {
			task.Status = model.TaskStatusLocalFailure
			ownerCtx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 5*time.Second)
			_ = taskbase.FailTaskWithSettlement(ownerCtx, task, "Midjourney submission claim failed")
			cancel()
		}
		return nil, provider.MidjourneyErrorWrapper(provider.MjRequestError, "midjourney_submission_claim_failed")
	}
	return task, nil
}

type midjourneyFingerprintEnvelope struct {
	Path      string `json:"path"`
	UserID    int    `json:"user_id"`
	ChannelID int    `json:"channel_id"`
	Action    string `json:"action"`
	Model     string `json:"model"`
	Body      string `json:"body"`
}

func midjourneyTaskRequestFingerprint(c *gin.Context, view *model.Midjourney) string {
	path := ""
	userID := 0
	channelID := 0
	if c != nil {
		userID = c.GetInt("id")
		channelID = c.GetInt("channel_id")
		if c.Request != nil && c.Request.URL != nil {
			path = c.Request.URL.Path
		}
	}
	if view != nil {
		if userID == 0 {
			userID = view.UserId
		}
		if channelID == 0 {
			channelID = view.ChannelId
		}
	}

	// 规范化只服务于 owner 身份比较；provider 仍读取同一份 canonical body 出站。
	requestBody, _ := common.GetCanonicalRequestBody(c)
	canonicalBody := canonicalizeMidjourneyFingerprintJSON(requestBody)
	envelope := midjourneyFingerprintEnvelope{
		Path:      path,
		UserID:    userID,
		ChannelID: channelID,
		Body:      string(canonicalBody),
	}
	if view != nil {
		envelope.Action = view.Action
		envelope.Model = CoverActionToModelName(view.Action, view.Mode)
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		// The envelope contains only strings and integers, so this is defensive.
		encoded = []byte(fmt.Sprintf("%s\x00%d\x00%d\x00%s\x00%s\x00%s", path, userID, channelID, envelope.Action, envelope.Model, envelope.Body))
	}
	digest := sha256.Sum256(encoded)
	return fmt.Sprintf("%x", digest[:])
}

func canonicalizeMidjourneyFingerprintJSON(body []byte) []byte {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return body
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return body
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return body
	}
	return canonical
}

func acceptMidjourneyTaskOwner(c *gin.Context, task *model.Task, view *model.Midjourney) *provider.MidjourneyResponse {
	if c == nil || task == nil || view == nil || strings.TrimSpace(view.MjId) == "" {
		return provider.MidjourneyErrorWrapper(provider.MjRequestError, "provider_task_id_missing")
	}
	provisionalOwnerID := task.OwnerID
	model.SetTaskProviderID(task, view.MjId)
	legacyFingerprint := legacyMidjourneyTaskRequestFingerprint(c)
	ownerCtx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 5*time.Second)
	defer cancel()
	var result model.TaskMutationResult
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		result, err = model.AcceptTaskSubmissionWithLegacyFingerprint(ownerCtx, task, view.MjId, legacyFingerprint)
		if result.Outcome != model.TaskMutationDefinitelyNotApplied || err == nil {
			break
		}
	}
	if err != nil || result.Outcome != model.TaskMutationApplied {
		if result.Outcome == model.TaskMutationCommitUnknown {
			_, _ = model.PreserveTaskSubmissionHandle(ownerCtx, task, view.MjId)
		}
		return provider.MidjourneyErrorWrapper(provider.MjRequestError, "persist_midjourney_acceptance_failed")
	}
	if task.OwnerID != provisionalOwnerID {
		taskprogress.ActivateUpdateTaskBulk()
		return nil
	}

	view.Id = int(task.ID)
	view.UserId = task.UserId
	view.TokenID = task.TokenID
	view.ChannelId = task.ChannelId
	task.Data = model.EncodeMidjourneyTaskData(view)
	task.SubmitTime = view.SubmitTime
	task.StartTime = view.StartTime
	task.FinishTime = view.FinishTime
	task.FailReason = view.FailReason
	progress := strings.TrimSuffix(strings.TrimSpace(view.Progress), "%")
	if parsed, parseErr := strconv.Atoi(progress); parseErr == nil && parsed >= 0 && parsed <= 100 {
		task.Progress = parsed
	}
	switch strings.ToUpper(strings.TrimSpace(view.Status)) {
	case string(model.TaskStatusSuccess):
		task.Status = model.TaskStatusSuccess
	case string(model.TaskStatusFailure):
		task.Status = model.TaskStatusFailure
	case string(model.TaskStatusCancel):
		task.Status = model.TaskStatusCancel
	case string(model.TaskStatusQueued):
		task.Status = model.TaskStatusQueued
	case string(model.TaskStatusInProgress):
		task.Status = model.TaskStatusInProgress
	default:
		task.Status = model.TaskStatusSubmitted
	}
	if task.Status == model.TaskStatusSuccess || task.Status == model.TaskStatusFailure || task.Status == model.TaskStatusCancel {
		task.Progress = 100
		if _, err := taskbase.FinalizeTaskSettlement(ownerCtx, task); err != nil {
			return provider.MidjourneyErrorWrapper(provider.MjRequestError, "finalize_midjourney_task_failed")
		}
	} else if _, err := model.SaveTaskPollSnapshot(ownerCtx, task, time.Now()); err != nil {
		return provider.MidjourneyErrorWrapper(provider.MjRequestError, "persist_midjourney_snapshot_failed")
	}
	taskprogress.ActivateUpdateTaskBulk()
	return nil
}

func legacyMidjourneyTaskRequestFingerprint(c *gin.Context) string {
	path := ""
	if c != nil && c.Request != nil && c.Request.URL != nil {
		path = c.Request.URL.Path
	}
	requestBody, _ := common.GetCanonicalRequestBody(c)
	digest := sha256.Sum256(append([]byte(path+"\x00"), requestBody...))
	return fmt.Sprintf("%x", digest[:])
}

func parseMidjourneyUploadSuccess(responseBody []byte) (provider.MidjourneyUploadResponse, bool) {
	var response provider.MidjourneyUploadResponse
	if err := json.Unmarshal(responseBody, &response); err != nil || response.Code != 1 || len(response.Result) == 0 {
		return response, false
	}
	for _, imageURL := range response.Result {
		candidate := strings.TrimSpace(imageURL)
		parsed, err := url.ParseRequestURI(candidate)
		if candidate == "" || err != nil || !parsed.IsAbs() || parsed.Host == "" {
			return response, false
		}
	}
	return response, true
}

func completeMidjourneyUploadOwner(c *gin.Context, task *model.Task, view *model.Midjourney) *provider.MidjourneyResponse {
	if c == nil || task == nil || view == nil || task.ID <= 0 {
		return provider.MidjourneyErrorWrapper(provider.MjRequestError, "task_owner_required")
	}
	now := time.Now().UnixNano() / int64(time.Millisecond)
	view.Id = int(task.ID)
	view.UserId = task.UserId
	view.TokenID = task.TokenID
	view.ChannelId = task.ChannelId
	view.MjId = ""
	view.Status = string(model.TaskStatusSuccess)
	view.Progress = "100%"
	view.StartTime = now
	view.FinishTime = now
	view.FailReason = ""
	task.Status = model.TaskStatusSuccess
	task.Progress = 100
	task.StartTime = now
	task.FinishTime = now
	task.FailReason = ""
	task.Data = model.EncodeMidjourneyTaskData(view)
	ownerCtx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 5*time.Second)
	defer cancel()
	finalized, err := taskbase.FinalizeTaskSettlement(ownerCtx, task)
	if err != nil || !finalized.Handled {
		return provider.MidjourneyErrorWrapper(provider.MjRequestError, "finalize_midjourney_upload_failed")
	}
	return nil
}

func closeMidjourneyTaskOwner(c *gin.Context, task *model.Task, status model.TaskStatus, reason string) {
	if c == nil || task == nil {
		return
	}
	task.Status = status
	task.FailReason = reason
	task.Progress = 100
	ownerCtx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 5*time.Second)
	_ = taskbase.FailTaskWithSettlement(ownerCtx, task, reason)
	cancel()
}

func getMJProviderWithRequest(c *gin.Context, relayMode int, request *provider.MidjourneyRequest) (*provider.MidjourneyProvider, *provider.MidjourneyResponse) {
	mjModel := c.GetString("mj_model")
	midjourneyModel, mjErr, _ := GetMjRequestModel(relayMode, request, mjModel)
	if mjErr != nil {
		return nil, MidjourneyErrorFromInternal(mjErr.Code, mjErr.Description)
	}
	if midjourneyModel == "" {
		return nil, MidjourneyErrorFromInternal(provider.MjErrorUnknown, "无效的请求, 无法解析模型")
	}

	return getMJProvider(c, midjourneyModel)
}

func getMJProviderWithChannelId(c *gin.Context, channelId int) (*provider.MidjourneyProvider, *provider.MidjourneyResponse) {
	channel, err := model.GetChannelIncarnationByID(c.Request.Context(), channelId)
	if err != nil {
		return nil, MidjourneyErrorFromInternal(provider.MjErrorUnknown, "无法读取任务 owner 渠道")
	}
	if err := channel.ValidateRuntimeConfigJSON(); err != nil {
		return nil, MidjourneyErrorFromInternal(provider.MjErrorUnknown, "任务 owner 渠道配置无效")
	}
	baseProvider := providers.GetProvider(channel, c)
	midjourneyProvider, ok := baseProvider.(*provider.MidjourneyProvider)
	if !ok {
		return nil, MidjourneyErrorFromInternal(provider.MjErrorUnknown, "任务 owner 渠道 provider 无效")
	}
	c.Set("channel_id", channelId)
	c.Set("channel_type", channel.Type)
	return midjourneyProvider, nil
}

func getMJProvider(c *gin.Context, modelName string) (*provider.MidjourneyProvider, *provider.MidjourneyResponse) {
	baseProvider, _, err := relay.GetProvider(c, modelName)
	if err != nil {
		return nil, MidjourneyErrorFromInternal(provider.MjErrorUnknown, "无法获取provider:"+err.Error())
	}

	mjProvider, ok := baseProvider.(*provider.MidjourneyProvider)
	if !ok {
		return nil, MidjourneyErrorFromInternal(provider.MjErrorUnknown, "无效的请求, 无法获取midjourney provider")
	}

	return mjProvider, nil
}
