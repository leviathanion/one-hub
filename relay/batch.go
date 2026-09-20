package relay

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"one-api/common/config"
	"one-api/common/groupctx"
	"one-api/common/logger"
	"one-api/common/providerresponse"
	"one-api/common/requestctx"
	"one-api/common/requester"
	commonresponses "one-api/common/responses"
	"one-api/internal/billing"
	"one-api/middleware"
	"one-api/model"
	providersBase "one-api/providers/base"
	"one-api/relay/relay_util"
	"one-api/types"
)

// 这些限制约束代理扫描和证据内存，不代替上游 Batch 参数校验。
const batchScanBytes int64 = 200 << 20
const batchLineBytes = 8 << 20
const batchItemLimit = 10000
const batchRequestDeadline = 5 * time.Minute

type batchItemAdmission struct {
	Model                string            `json:"model"`
	BillingOriginalModel bool              `json:"billing_original_model,omitempty"`
	Endpoint             string            `json:"endpoint"`
	Ambiguous            bool              `json:"ambiguous,omitempty"`
	Slots                map[string]uint64 `json:"slots,omitempty"`
}
type openAIBatchData struct {
	TerminalStatus           string                              `json:"observed_terminal_status,omitempty"`
	PendingEvidence          map[string]*batchPendingEvidence    `json:"pending_evidence,omitempty"`
	EvidenceCapacityLimited  bool                                `json:"evidence_capacity_limited,omitempty"`
	InputFileID              string                              `json:"input_file_id"`
	Endpoint                 string                              `json:"endpoint"`
	Group                    string                              `json:"group"`
	Items                    map[string]*batchItemAdmission      `json:"items"`
	Slots                    map[string]uint64                   `json:"slots"`
	OutputFileID             string                              `json:"output_file_id,omitempty"`
	ErrorFileID              string                              `json:"error_file_id,omitempty"`
	Failures                 int                                 `json:"poll_failures,omitempty"`
	ObservedItems            int                                 `json:"observed_items,omitempty"`
	PricedItems              int                                 `json:"priced_items,omitempty"`
	Evidence                 map[string]*backgroundUsageEvidence `json:"evidence,omitempty"`
	LogUsage                 *billing.UsageSummary               `json:"log_usage,omitempty"`
	EvidenceDigest           string                              `json:"evidence_digest,omitempty"`
	EvidenceSummaryTruncated bool                                `json:"evidence_summary_truncated,omitempty"`
}
type batchEnvelope struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	OutputFileID string `json:"output_file_id"`
	ErrorFileID  string `json:"error_file_id"`
}

func (e *batchEnvelope) UnmarshalJSON(raw []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	// 计费状态投影失败不抹掉已经可识别的真实资源 ID。
	_ = json.Unmarshal(fields["id"], &e.ID)
	_ = json.Unmarshal(fields["status"], &e.Status)
	_ = json.Unmarshal(fields["output_file_id"], &e.OutputFileID)
	_ = json.Unmarshal(fields["error_file_id"], &e.ErrorFileID)
	return nil
}

type batchInputLine struct {
	CustomID string          `json:"custom_id"`
	Method   string          `json:"method"`
	URL      string          `json:"url"`
	Body     json.RawMessage `json:"body"`
}
type batchResultLine struct {
	CustomID string `json:"custom_id"`
	Response *struct {
		StatusCode int             `json:"status_code"`
		RequestID  string          `json:"request_id"`
		Body       json.RawMessage `json:"body"`
	} `json:"response"`
}

func batchData(task *model.Task) (openAIBatchData, error) {
	var d openAIBatchData
	err := json.Unmarshal(task.Data, &d)
	if err == nil && (d.Items == nil || d.Slots == nil) {
		err = errors.New("batch admission facts are missing")
	}
	return d, err
}
func encodeBatchData(d openAIBatchData) ([]byte, error) {
	raw, err := json.Marshal(d)
	if err == nil && len(raw) > model.BatchDataMaxBytes {
		err = errors.New("batch accounting metadata exceeds capacity")
	}
	return raw, err
}
func batchEndpointSupported(endpoint string) bool {
	switch endpoint {
	case "/v1/responses", "/v1/chat/completions", "/v1/completions", "/v1/embeddings", "/v1/moderations", "/v1/images/generations", "/v1/images/edits":
		return true
	}
	return false
}
func batchSlot(customID, role string) string {
	sum := sha256.Sum256([]byte(customID))
	return hex.EncodeToString(sum[:16]) + ":" + role
}

func BatchRelay(c *gin.Context) {
	if apiErr := middleware.ValidateCurrentPrincipal(c); apiErr != nil {
		renderStoredResponsesError(c, apiErr)
		return
	}
	if explicitChannelPinID(c) > 0 {
		if apiErr := middleware.RefreshLongLivedPrincipal(c); apiErr != nil {
			renderStoredResponsesError(c, apiErr)
			return
		}
		if c.GetInt("role") < config.RoleAdminUser {
			renderResourceError(c, "administrator permission is required", "permission_denied", http.StatusForbidden)
			return
		}
		RelayOnly(c)
		return
	}
	ioOwner, err := beginResourceHTTPIO(c)
	if err != nil {
		renderResourceError(c, "resource I/O deadlines are unavailable", "resource_io_unavailable", http.StatusInternalServerError)
		return
	}
	defer ioOwner.Close()
	if c.Request.Body != nil {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, resourceJSONBytes)
		defer c.Request.Body.Close()
	}
	path := c.Request.URL.EscapedPath()
	if path == "/v1/batches" {
		if c.Request.Method == http.MethodPost {
			createOpenAIBatch(c)
			return
		}
		if c.Request.Method == http.MethodGet {
			renderResourceError(c, "account-wide batch lists require an administrator channel", "unsupported_resource_list", http.StatusForbidden)
			return
		}
	}
	parts := strings.Split(strings.TrimPrefix(path, "/v1/batches/"), "/")
	if !strings.HasPrefix(path, "/v1/batches/") || len(parts) > 2 || (len(parts) == 1 && c.Request.Method != http.MethodGet) || (len(parts) == 2 && (parts[1] != "cancel" || c.Request.Method != http.MethodPost)) {
		renderResourceError(c, "batch operation is not implemented", "unsupported_resource_operation", http.StatusNotImplemented)
		return
	}
	id, err := url.PathUnescape(parts[0])
	if err != nil || id == "" || strings.ContainsAny(id, "/\\\x00") {
		renderResourceError(c, "invalid batch resource envelope", "invalid_request", http.StatusBadRequest)
		return
	}
	owner, err := model.GetResourceOwner(c.Request.Context(), "batch", id, c.GetInt("id"))
	if err != nil {
		renderResourceOwnerError(c, err)
		return
	}
	if owner.TaskOwnerID == nil {
		renderResourceOwnerError(c, model.ErrResourceOwnerNotFound)
		return
	}
	task, err := model.GetOpenAIBatchTask(c.Request.Context(), *owner.TaskOwnerID)
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		renderResourceOwnerError(c, err)
		return
	}
	if err == nil && (task.UserId != owner.UserID || task.ChannelId != owner.ChannelID) {
		renderResourceOwnerError(c, model.ErrResourceOwnerNotFound)
		return
	}
	if err != nil {
		task = nil
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), batchRequestDeadline)
	defer cancel()
	provider, adapter, err := batchProvider(c, owner.ChannelID)
	if err != nil {
		renderResourceError(c, "batch channel unavailable", "unsupported_resource_channel", http.StatusServiceUnavailable)
		return
	}
	response, apiErr := sendBatchRequest(ctx, c, provider, adapter, c.Request.Method, path, c.Request.URL.RawQuery, c.Request.Body)
	if apiErr != nil {
		renderStoredResponsesError(c, apiErr)
		return
	}
	if response == nil || response.Body == nil {
		renderResourceError(c, "empty batch response", "invalid_provider_response", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		raw, err := readBatchEnvelope(response.Body)
		if err != nil {
			renderResourceError(c, err.Error(), "resource_response_limit", http.StatusBadGateway)
			return
		}
		if task != nil {
			err = observeBatchFiles(ctx, task, raw)
		} else {
			err = authorizeRetainedBatchFiles(ctx, owner, raw)
		}
		if err != nil {
			renderResourceOwnerError(c, err)
			return
		}
		response.Body = io.NopCloser(bytes.NewReader(raw))
	}
	deliverBatchResponse(c, provider, response)
}

func batchProvider(c *gin.Context, channelID int) (providersBase.ProviderInterface, providersBase.BatchRelayInterface, error) {
	channel, err := fetchOwnerChannelById(c.Request.Context(), channelID)
	if err != nil {
		return nil, nil, err
	}
	p, _, err := prepareProviderForChannel(c, "", channel)
	if err != nil {
		return nil, nil, err
	}
	a, ok := p.(providersBase.BatchRelayInterface)
	if !ok {
		return nil, nil, errors.New("native batch adapter is unavailable")
	}
	if _, err = a.BuildBatchRelayURL("/v1/batches", ""); err != nil {
		return nil, nil, err
	}
	return p, a, nil
}

func sendBatchRequest(ctx context.Context, c *gin.Context, p providersBase.ProviderInterface, a providersBase.BatchRelayInterface, method, path, query string, body io.Reader) (*http.Response, *types.OpenAIErrorWithStatusCode) {
	address, err := a.BuildBatchRelayURL(path, query)
	if err != nil {
		return nil, resourceBatchError(err, "unsupported_resource_channel", http.StatusBadRequest)
	}
	r := p.GetRequester().ForHTTPProfile(requester.HTTPProfileLongStream)
	request, err := r.NewRequest(method, address, r.WithContext(ctx), r.WithBody(body), r.WithHeader(p.GetRequestHeaders()))
	if err != nil {
		return nil, resourceBatchError(err, "invalid_request", http.StatusBadRequest)
	}
	if c != nil && c.Request != nil {
		if err = requestctx.ApplyRegisteredExactWireRequestHeaders(request.Header, requestctx.NewHeaderSnapshot(c.Request.Header)); err != nil {
			return nil, resourceBatchError(err, "invalid_request", http.StatusBadRequest)
		}
	}
	return r.SendRequestRawNoRedirect(request)
}
func resourceBatchError(err error, code string, status int) *types.OpenAIErrorWithStatusCode {
	return &types.OpenAIErrorWithStatusCode{StatusCode: status, OpenAIError: types.OpenAIError{Message: err.Error(), Type: "invalid_request_error", Code: code}, LocalError: true}
}
func readBatchEnvelope(r io.Reader) ([]byte, error) {
	b, e := io.ReadAll(io.LimitReader(r, resourceJSONBytes+1))
	if e != nil {
		return nil, e
	}
	if int64(len(b)) > resourceJSONBytes {
		return nil, errors.New("batch envelope exceeds local capacity")
	}
	return b, nil
}
func deliverBatchResponse(c *gin.Context, p providersBase.ProviderInterface, response *http.Response) {
	if response.StatusCode >= 400 {
		apiErr := requester.HandleErrorResp(response, p.GetRequester().ErrorHandler, p.GetRequester().PrefixProviderErrors, true)
		renderStoredResponsesError(c, apiErr)
		return
	}
	response.Header = providerresponse.FilterCredentialHeaders(response.Header, requestctx.ProviderCredentials(c)...)
	if apiErr := responseMultipart(c, response, providerresponse.Policy{Operation: providerresponse.OperationRawRelay, DataPath: providerresponse.DataPathExactWire, BodyUnmodified: true, PreserveRedirect: true}); apiErr != nil {
		renderStoredResponsesError(c, apiErr)
	}
}

func createOpenAIBatch(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), batchRequestDeadline)
	defer cancel()
	raw, err := readBatchEnvelope(c.Request.Body)
	if err != nil {
		renderResourceError(c, err.Error(), "resource_request_limit", http.StatusRequestEntityTooLarge)
		return
	}
	var request struct {
		InputFileID string `json:"input_file_id"`
		Endpoint    string `json:"endpoint"`
	}
	if json.Unmarshal(raw, &request) != nil || request.InputFileID == "" {
		renderResourceError(c, "batch input_file_id is required for authorization", "invalid_request", http.StatusBadRequest)
		return
	}
	if !batchEndpointSupported(request.Endpoint) {
		renderResourceError(c, "batch endpoint has no implemented ownership and evidence adapter", "unsupported_batch_endpoint", http.StatusBadRequest)
		return
	}
	input, err := model.GetResourceOwner(ctx, "file", request.InputFileID, c.GetInt("id"))
	if err != nil {
		renderResourceOwnerError(c, err)
		return
	}
	p, a, err := batchProvider(c, input.ChannelID)
	if err != nil {
		renderResourceError(c, "batch channel unavailable", "unsupported_resource_channel", http.StatusServiceUnavailable)
		return
	}
	if apiErr := authorizeResourceCreationChannel(c, p.GetChannel()); apiErr != nil {
		renderStoredResponsesError(c, apiErr)
		return
	}
	c.Set("channel_id", input.ChannelID)
	d := openAIBatchData{InputFileID: request.InputFileID, Endpoint: request.Endpoint, Group: groupctx.CurrentRoutingGroup(c), Items: map[string]*batchItemAdmission{}, Slots: map[string]uint64{}}
	reserved, err := scanBatchInput(ctx, c, p, &d)
	if err != nil {
		renderResourceError(c, err.Error(), "batch_admission_failed", http.StatusBadRequest)
		return
	}
	namespace, scope, err := model.ConservativeResponseOwnerIdentity(p.GetChannel())
	if err != nil {
		renderResourceOwnerError(c, err)
		return
	}
	fingerprint := sha256.Sum256(raw)
	task := &model.Task{OwnerID: uuid.NewString(), Platform: model.TaskPlatformOpenAIBatch, UserId: c.GetInt("id"), TokenID: c.GetInt("token_id"), ChannelId: input.ChannelID, Status: model.TaskStatusSubmitted, SubmitTime: time.Now().Unix(), ProviderNamespace: namespace, ProviderTaskScopeIncarnation: scope, RequestFingerprint: hex.EncodeToString(fingerprint[:]), ReservedQuota: reserved}
	deadline, _ := ctx.Deadline()
	spec := model.ResourceOwnerReservation{UserID: task.UserId, TokenID: task.TokenID, ChannelID: task.ChannelId, ProviderNamespace: namespace, ProviderScope: scope, TaskOwnerID: task.OwnerID, SubmitDeadline: deadline}
	specs := []model.ResourceOwnerReservation{}
	for _, role := range []string{"batch", "output", "error"} {
		s := spec
		s.Slot = role
		s.Kind = "file"
		if role == "batch" {
			s.Kind = "batch"
		}
		specs = append(specs, s)
	}
	for customID, item := range d.Items {
		for role := range item.Slots {
			s := spec
			s.Slot = batchSlot(customID, role)
			switch {
			case role == "response":
				s.Kind = "response"
			case role == "stored_chat":
				s.Kind = "stored_chat"
			default:
				s.Kind = "chat_audio"
			}
			specs = append(specs, s)
		}
	}
	owners, err := model.NewResourceOwnerRepository(model.DB).ReserveMany(ctx, specs)
	if err != nil {
		renderResourceOwnerError(c, err)
		return
	}
	for _, o := range owners {
		d.Slots[*o.Slot] = o.ID
	}
	for customID, item := range d.Items {
		for role := range item.Slots {
			item.Slots[role] = d.Slots[batchSlot(customID, role)]
		}
	}
	defer func() {
		if task.ProviderState != model.TaskProviderStateAccepted && task.ProviderState != model.TaskProviderStateClosed {
			cleanup, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer done()
			_ = model.NewResourceOwnerRepository(model.DB).ReleaseSubmittingTaskSlots(cleanup, task.UserId, task.OwnerID)
		}
	}()
	task.Data, err = encodeBatchData(d)
	if err != nil || len(task.Data) > model.BatchDataMaxBytes-2048 {
		if err == nil {
			err = errors.New("batch admission metadata leaves no bounded observation/settlement space")
		}
		renderResourceError(c, err.Error(), "batch_metadata_limit", http.StatusRequestEntityTooLarge)
		return
	}
	admit, err := model.CreateTaskBillingOwner(ctx, task)
	if err != nil || admit.Outcome != model.BillingBalanceCommitted {
		renderStoredResponsesError(c, relay_util.BillingAPIError(err, "batch_admission_failed", http.StatusServiceUnavailable))
		return
	}
	defer func() {
		if task.ProviderState != model.TaskProviderStateAccepted && task.ProviderState != model.TaskProviderStateClosed {
			closeBatchWithoutHandle(ctx, task, "submission has no durable accepted handle")
		}
	}()
	claim, err := model.ClaimTaskSubmission(ctx, task, uuid.NewString())
	if err != nil || claim.Outcome != model.TaskMutationApplied {
		renderResourceError(c, "batch submission claim unavailable", "batch_submission_failed", http.StatusServiceUnavailable)
		return
	}
	response, apiErr := sendBatchRequest(ctx, c, p, a, http.MethodPost, "/v1/batches", c.Request.URL.RawQuery, bytes.NewReader(raw))
	if apiErr != nil {
		renderStoredResponsesError(c, apiErr)
		return
	}
	if response == nil || response.Body == nil {
		renderResourceError(c, "empty batch response", "invalid_provider_response", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		body, err := readBatchEnvelope(response.Body)
		if err != nil {
			renderResourceError(c, err.Error(), "resource_response_limit", http.StatusBadGateway)
			return
		}
		var envelope batchEnvelope
		if json.Unmarshal(body, &envelope) != nil || envelope.ID == "" {
			renderResourceError(c, "batch response has no identity", "resource_owner_commit_failed", http.StatusBadGateway)
			return
		}
		ids := make([]uint64, 0, len(owners)-1)
		for _, o := range owners {
			if *o.Slot != "batch" {
				ids = append(ids, o.ID)
			}
		}
		acceptCtx, done := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		accepted, err := model.AcceptOpenAIBatchSubmission(acceptCtx, task, envelope.ID, d.Slots["batch"], ids)
		done()
		if err != nil || accepted.Outcome != model.TaskMutationApplied {
			renderResourceError(c, "batch acceptance could not be committed", "resource_owner_commit_failed", http.StatusServiceUnavailable)
			return
		}
		if err = observeBatchFiles(ctx, task, body); err != nil {
			renderResourceOwnerError(c, err)
			return
		}
		response.Body = io.NopCloser(bytes.NewReader(body))
	}
	deliverBatchResponse(c, p, response)
}

func closeBatchWithoutHandle(parent context.Context, task *model.Task, reason string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer cancel()
	task.Status = model.TaskStatusUnknown
	task.FailReason = reason
	result, err := model.FinalizeOpenAIBatchBillingOwner(ctx, task, 0, "cancel", task.Data)
	if err == nil {
		ProjectAsyncTaskSettlement(ctx, task, result)
	}
	if err != nil {
		logger.LogError(ctx, "batch close failed: "+err.Error())
	}
}

func batchFileResponse(ctx context.Context, p providersBase.ProviderInterface, id string) (*http.Response, error) {
	builder, ok := p.(providersBase.ResourceRelayURLBuilder)
	if !ok {
		return nil, errors.New("file adapter unavailable")
	}
	address, err := builder.BuildResourceRelayURL("/v1/files/"+url.PathEscape(id)+"/content", "")
	if err != nil {
		return nil, err
	}
	r := p.GetRequester().ForHTTPProfile(requester.HTTPProfileLongStream)
	req, err := r.NewRequest(http.MethodGet, address, r.WithContext(ctx), r.WithHeader(p.GetRequestHeaders()))
	if err != nil {
		return nil, err
	}
	response, apiErr := r.SendRequestRawNoRedirect(req)
	if apiErr != nil {
		return nil, apiErr
	}
	if response == nil || response.Body == nil {
		return nil, errors.New("file response is empty")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		response.Body.Close()
		return nil, fmt.Errorf("batch file upstream status %d", response.StatusCode)
	}
	return response, nil
}

func scanBatchInput(ctx context.Context, c *gin.Context, p providersBase.ProviderInterface, d *openAIBatchData) (int64, error) {
	response, err := batchFileResponse(ctx, p, d.InputFileID)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	var reserve int64
	slotCount := 3 // Batch 本身与 output/error File 的提交前有界容量。
	err = readBatchLines(response.Body, func(raw []byte) error {
		if len(bytes.TrimSpace(raw)) == 0 {
			return nil
		}
		var line batchInputLine
		if err := json.Unmarshal(raw, &line); err != nil {
			return errors.New("batch line is not a readable authorization envelope")
		}
		if len(line.CustomID) > 256 {
			return errors.New("batch custom_id exceeds local metadata capacity")
		}
		if line.URL != d.Endpoint || line.Method != http.MethodPost {
			return errors.New("batch line operation has no matching native adapter")
		}
		var body map[string]any
		decoder := json.NewDecoder(bytes.NewReader(line.Body))
		decoder.UseNumber()
		if decoder.Decode(&body) != nil {
			return errors.New("batch body is not a readable authorization envelope")
		}
		name, _ := body["model"].(string)
		if name == "" {
			return errors.New("batch model is required for local authorization")
		}
		if err := middleware.EnsureTokenModelAllowed(c, name); err != nil {
			return err
		}
		if err := middleware.EnsureLongLivedChannelAllowed(c, name); err != nil {
			return err
		}
		mapped, err := p.ModelMappingHandler(name)
		if err != nil {
			return err
		}
		billingOriginal := strings.HasPrefix(mapped, "+")
		if strings.TrimPrefix(mapped, "+") != name {
			return errors.New("batch model mapping would require rewriting the input file")
		}
		operation := ""
		if line.URL == "/v1/responses" {
			operation = "responses"
		}
		if line.URL == "/v1/chat/completions" {
			operation = "chat"
		}
		if strings.HasPrefix(line.URL, "/v1/images/") {
			operation = "images"
		}
		if operation != "" {
			if err := prepareBatchReferences(c, body, operation); err != nil {
				return err
			}
		}
		if previous, _ := body["previous_response_id"].(string); operation == "responses" && previous != "" {
			owner, err := model.GetResponseOwner(ctx, previous, c.GetInt("id"))
			if err != nil || owner.ChannelID != p.GetChannel().Id {
				return errors.New("batch response reference is not authorized on the input file channel")
			}
		}
		item := &batchItemAdmission{Model: name, BillingOriginalModel: billingOriginal, Endpoint: line.URL, Slots: map[string]uint64{}}
		if line.URL == "/v1/responses" {
			item.Slots["response"] = 0
		}
		if line.URL == "/v1/chat/completions" {
			// 所有真实 completion ID 都保留最小归属；store 是否可查询由上游判断。
			item.Slots["stored_chat"] = 0
			_, audio := body["audio"]
			if modalities, ok := body["modalities"].([]any); ok {
				for _, m := range modalities {
					if value, ok := m.(string); ok && value == "audio" {
						audio = true
					}
				}
			}
			if audio {
				n := 1
				if v, ok := body["n"].(json.Number); ok {
					if parsed, e := v.Int64(); e == nil && parsed > 0 {
						if parsed > batchItemLimit {
							return errors.New("batch audio derived capacity exceeds limit")
						}
						n = int(parsed)
					}
				}
				for i := 0; i < n; i++ {
					item.Slots[fmt.Sprintf("audio:%d", i)] = 0
				}
			}
		}
		if old := d.Items[line.CustomID]; old != nil {
			old.Ambiguous = true
			for role := range item.Slots {
				if _, exists := old.Slots[role]; !exists {
					slotCount++
				}
				if slotCount > model.ResourceOwnerLimit {
					return model.ErrResourceOwnerCapacity
				}
				old.Slots[role] = 0
			}
		} else {
			slotCount += len(item.Slots)
			if slotCount > model.ResourceOwnerLimit {
				return model.ErrResourceOwnerCapacity
			}
			d.Items[line.CustomID] = item
		}
		quota, err := relay_util.NewPricedQuota(c, name, 1000)
		if err != nil {
			return err
		}
		amount, err := quota.ReservationQuota()
		if err != nil {
			return err
		}
		if int64(amount) > math.MaxInt64-reserve {
			return errors.New("batch reservation exceeds local accounting range")
		}
		reserve += int64(amount)
		return nil
	})
	return reserve, err
}

func readBatchLines(reader io.Reader, visit func([]byte) error) error {
	limited := &io.LimitedReader{R: reader, N: batchScanBytes + 1}
	r := bufio.NewReaderSize(limited, 64<<10)
	count := 0
	for {
		line, err := readBoundedBatchLine(r)
		if len(line) > 0 {
			count++
			if count > batchItemLimit {
				return errors.New("batch item count exceeds local observation capacity")
			}
			if e := visit(line); e != nil {
				return e
			}
		}
		if limited.N <= 0 {
			return errors.New("batch file exceeds local scan capacity")
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func prepareBatchReferences(c *gin.Context, body map[string]any, operation string) error {
	var refs []commonresponses.ResourceReference
	switch operation {
	case "responses":
		if err := validateResponsesPromptSurface(body["prompt"]); err != nil {
			return err
		}
		refs = commonresponses.ExtractAccountScopedResourceReferences(map[string]any{"input": body["input"], "tools": body["tools"], "conversation": body["conversation"]})
	case "chat":
		refs = commonresponses.InputResourceReferences(body["messages"])
		refs = append(refs, commonresponses.ToolResourceReferences(body["tools"])...)
		refs = append(refs, commonresponses.MediaResourceReferences(body, "chat")...)
	case "images":
		refs = commonresponses.MediaResourceReferences(body, "images")
	}
	policy := relay_util.ResourceReferencePolicy{Context: c}
	for _, ref := range refs {
		if err := policy.AuthorizeResourceReference(ref.Kind, ref.ID); err != nil {
			return err
		}
	}
	return nil
}
func readBoundedBatchLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		part, err := r.ReadSlice('\n')
		if len(line)+len(part) > batchLineBytes {
			return nil, errors.New("batch line exceeds local observation capacity")
		}
		line = append(line, part...)
		if err != bufio.ErrBufferFull {
			return line, err
		}
	}
}
