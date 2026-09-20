package relay

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"one-api/common/logger"
	"one-api/model"
	"one-api/types"
)

// 仅真实 Batch 回执里的 output/error 文件可以消耗接受前的派生槽位。
func observeBatchFiles(parent context.Context, task *model.Task, raw []byte) error {
	var envelope batchEnvelope
	if json.Unmarshal(raw, &envelope) != nil || envelope.ID != model.TaskProviderID(task) {
		return errors.New("batch response identity does not match the authorized task")
	}
	d, err := batchData(task)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer cancel()
	for role, id := range map[string]string{"output": envelope.OutputFileID, "error": envelope.ErrorFileID} {
		if id == "" {
			continue
		}
		reservation := d.Slots[role]
		if reservation == 0 {
			return model.ErrResourceReservationExpired
		}
		if err := model.BindOpenAIBatchFile(ctx, task, reservation, id, d.Endpoint, role); err != nil {
			return err
		}
		if role == "output" {
			d.OutputFileID = id
		} else {
			d.ErrorFileID = id
		}
	}
	if task.ProviderState == model.TaskProviderStateAccepted {
		encoded, err := encodeBatchData(d)
		if err != nil {
			return err
		}
		task.Data = encoded
		next := time.Now().Add(model.TaskPollInterval)
		if held := time.Unix(task.NextActionAt, 0); held.After(next) {
			next = held
		}
		if _, err = model.SaveOpenAIBatchSnapshot(ctx, task, next); err != nil {
			logger.LogError(ctx, "batch file observation snapshot unavailable: "+err.Error())
		}
	}
	return nil
}

// PrepareBatchFileDelivery 只在逐行原始字节交付前建立已知派生资源归属，
// 不为计费读取完文件，不重排/改写 JSONL，也不判断 Batch 业务终态。
func PrepareBatchFileDelivery(c *gin.Context, owner *model.ResourceOwner, response *http.Response) *types.OpenAIErrorWithStatusCode {
	if owner == nil || owner.TaskOwnerID == nil || owner.Slot == nil || (*owner.Slot != "output" && *owner.Slot != "error") {
		return nil
	}
	// 文件内容不可原地修改。已经完整观察的同一文件重读不再依赖更短
	// 留存的 Task/子 owner；此事实不能用于认领或延长任何子资源权限。
	if owner.BatchResultObserved {
		return nil
	}
	if owner.BatchResultEndpoint != "/v1/responses" && owner.BatchResultEndpoint != "/v1/chat/completions" {
		return nil
	}
	task, err := model.GetOpenAIBatchTask(c.Request.Context(), *owner.TaskOwnerID)
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return resourceBatchError(errors.New("batch result ownership source is unavailable"), "resource_owner_unavailable", http.StatusServiceUnavailable)
	}
	if err != nil {
		task = nil
	}
	if task != nil && (task.UserId != owner.UserID || task.ChannelId != owner.ChannelID) {
		return resourceBatchError(model.ErrResourceOwnerConflict, "resource_owner_conflict", http.StatusForbidden)
	}
	d := openAIBatchData{Endpoint: owner.BatchResultEndpoint}
	if task != nil {
		d, err = batchData(task)
		if err != nil {
			return resourceBatchError(err, "resource_owner_unavailable", http.StatusServiceUnavailable)
		}
	}
	if c.Request.Header.Get("Range") != "" || response.StatusCode == http.StatusPartialContent {
		return resourceBatchError(errors.New("partial batch result delivery with derived resources is not implemented"), "unsupported_batch_result_range", http.StatusBadRequest)
	}
	if encoding := response.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		return resourceBatchError(errors.New("compressed batch resource observation is not implemented"), "unsupported_batch_result_encoding", http.StatusBadGateway)
	}
	response.Body = &batchOwnedResultReader{source: response.Body, reader: bufio.NewReaderSize(response.Body, 64<<10), ctx: c.Request.Context(), task: task, data: d, owner: owner}
	return nil
}

type batchOwnedResultReader struct {
	source                io.ReadCloser
	reader                *bufio.Reader
	ctx                   context.Context
	task                  *model.Task
	data                  openAIBatchData
	owner                 *model.ResourceOwner
	pending               []byte
	terminal              error
	count                 int
	bytes                 int64
	completionRecorded    bool
	observationIncomplete bool
}

func (r *batchOwnedResultReader) Close() error { return r.source.Close() }
func (r *batchOwnedResultReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(r.pending) > 0 {
		n := copy(p, r.pending)
		r.pending = r.pending[n:]
		return n, nil
	}
	if r.terminal != nil {
		if errors.Is(r.terminal, io.EOF) {
			r.recordCompleteObservation()
		}
		return 0, r.terminal
	}
	line, err := readBoundedBatchLine(r.reader)
	if len(line) > 0 {
		r.count++
	}
	r.bytes += int64(len(line))
	if r.bytes > resourceDownloadBytes {
		return 0, errors.New("batch resource observation exceeds local capacity")
	}
	if len(bytes.TrimSpace(line)) > 0 {
		var projected batchResultLine
		if json.Unmarshal(line, &projected) != nil {
			r.observationIncomplete = true
		}
		var bindErr error
		if r.task != nil {
			bindErr = bindBatchResultResources(r.ctx, r.task, r.data, line)
		} else {
			bindErr = authorizeRetainedBatchResult(r.ctx, r.owner, line)
		}
		if bindErr != nil {
			r.terminal = bindErr
			return 0, bindErr
		}
	}
	r.pending = line
	r.terminal = err
	if len(line) > 0 {
		n := copy(p, r.pending)
		r.pending = r.pending[n:]
		return n, nil
	}
	if errors.Is(err, io.EOF) {
		r.recordCompleteObservation()
	}
	return 0, err
}

func (r *batchOwnedResultReader) recordCompleteObservation() {
	if r.completionRecorded || r.owner == nil || r.observationIncomplete {
		return
	}
	r.completionRecorded = true
	recordBatchFileObserved(r.ctx, r.owner)
}

func recordBatchFileObserved(parent context.Context, owner *model.ResourceOwner) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer cancel()
	if err := model.MarkOpenAIBatchFileObserved(ctx, owner); err != nil {
		logger.LogWarn(ctx, "batch file observation marker unavailable: "+err.Error())
	}
}

func bindBatchResultResources(ctx context.Context, task *model.Task, data openAIBatchData, raw []byte) error {
	var line batchResultLine
	if json.Unmarshal(raw, &line) != nil {
		return nil
	}
	if line.Response == nil || line.Response.StatusCode < 200 || line.Response.StatusCode >= 300 {
		return nil
	}
	refs := batchResultResourceReferences(data.Endpoint, line.Response.Body)
	if len(refs) == 0 {
		return nil
	}
	item := data.Items[line.CustomID]
	if item == nil {
		return errors.New("batch resource result has no admitted custom_id")
	}
	for _, ref := range refs {
		slot := item.Slots[ref.role]
		if slot == 0 {
			return model.ErrResourceReservationExpired
		}
		if ref.kind == "response" {
			if err := model.BindOpenAIBatchResponse(ctx, task, slot, batchSlot(line.CustomID, ref.role), ref.id); err != nil {
				return err
			}
		} else if _, err := model.BindResourceOwner(ctx, slot, task.UserId, ref.id); err != nil {
			return err
		}
	}
	return nil
}

type batchResultResourceReference struct{ kind, role, id string }

func batchResultResourceReferences(endpoint string, raw []byte) []batchResultResourceReference {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return nil
	}
	var id string
	_ = json.Unmarshal(fields["id"], &id)
	var refs []batchResultResourceReference
	if endpoint == "/v1/responses" && id != "" {
		return []batchResultResourceReference{{"response", "response", id}}
	}
	if endpoint != "/v1/chat/completions" {
		return nil
	}
	if id != "" {
		refs = append(refs, batchResultResourceReference{"stored_chat", "stored_chat", id})
	}
	var choices []json.RawMessage
	_ = json.Unmarshal(fields["choices"], &choices)
	for _, rawChoice := range choices {
		var choice map[string]json.RawMessage
		if json.Unmarshal(rawChoice, &choice) != nil {
			continue
		}
		var message, audio map[string]json.RawMessage
		if json.Unmarshal(choice["message"], &message) != nil || json.Unmarshal(message["audio"], &audio) != nil {
			continue
		}
		var audioID string
		if json.Unmarshal(audio["id"], &audioID) != nil || audioID == "" {
			continue
		}
		role := "audio:unattributable"
		var index int
		if json.Unmarshal(choice["index"], &index) == nil {
			role = fmt.Sprintf("audio:%d", index)
		}
		refs = append(refs, batchResultResourceReference{"chat_audio", role, audioID})
	}
	return refs
}

func authorizeRetainedBatchResult(ctx context.Context, file *model.ResourceOwner, raw []byte) error {
	var line batchResultLine
	if json.Unmarshal(raw, &line) != nil || line.Response == nil || line.Response.StatusCode < 200 || line.Response.StatusCode >= 300 {
		return nil
	}
	for _, ref := range batchResultResourceReferences(file.BatchResultEndpoint, line.Response.Body) {
		if ref.kind == "response" {
			owner, err := model.GetResponseOwner(ctx, ref.id, file.UserID)
			if err != nil {
				return err
			}
			if owner.ChannelID != file.ChannelID || owner.BatchOwnerID == nil || *owner.BatchOwnerID != *file.TaskOwnerID {
				return model.ErrResourceOwnerConflict
			}
		} else {
			owner, err := model.GetResourceOwner(ctx, ref.kind, ref.id, file.UserID)
			if err != nil {
				return err
			}
			if owner.ChannelID != file.ChannelID || owner.TaskOwnerID == nil || *owner.TaskOwnerID != *file.TaskOwnerID {
				return model.ErrResourceOwnerConflict
			}
		}
	}
	return nil
}

func authorizeRetainedBatchFiles(ctx context.Context, batch *model.ResourceOwner, raw []byte) error {
	var envelope batchEnvelope
	if json.Unmarshal(raw, &envelope) != nil {
		return nil
	}
	for role, id := range map[string]string{"output": envelope.OutputFileID, "error": envelope.ErrorFileID} {
		if id == "" {
			continue
		}
		observedID := batch.BatchOutputFileID
		if role == "error" {
			observedID = batch.BatchErrorFileID
		}
		if observedID == id {
			continue
		}
		owner, err := model.GetResourceOwner(ctx, "file", id, batch.UserID)
		if err != nil {
			return err
		}
		if owner.ChannelID != batch.ChannelID || owner.TaskOwnerID == nil || *owner.TaskOwnerID != *batch.TaskOwnerID {
			return model.ErrResourceOwnerConflict
		}
	}
	return nil
}
