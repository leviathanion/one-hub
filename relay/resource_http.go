package relay

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"one-api/common"
	"one-api/common/config"
	"one-api/common/groupctx"
	"one-api/common/logger"
	"one-api/common/providerresponse"
	"one-api/common/requestctx"
	"one-api/common/requester"
	responsescommon "one-api/common/responses"
	"one-api/middleware"
	"one-api/model"
	providersBase "one-api/providers/base"
	"one-api/types"

	"github.com/gin-gonic/gin"
)

const (
	resourceJSONBytes      int64 = 4 << 20
	resourceFileBytes      int64 = (512 << 20) + (1 << 20)
	resourcePartBytes      int64 = (64 << 20) + (1 << 20)
	resourceDownloadBytes  int64 = 8 << 30
	resourceRequestTimeout       = time.Hour
)

type resourceHTTPOperation struct {
	kind, id, createKind                          string
	rootCreate, topList, scanItems, observeDelete bool
	uploadPart, contentDownload                   bool
}

// ResourceRelay is the explicit native resource surface. Ownership is an
// authorization fact, never a projection of whether the upstream object exists.
func ResourceRelay(c *gin.Context) {
	if apiErr := middleware.ValidateCurrentPrincipal(c); apiErr != nil {
		renderStoredResponsesError(c, apiErr)
		return
	}
	// A raw administrator request has a separate, explicit authorization source.
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
		renderResourceError(c, "resource transport cannot enforce I/O deadlines", "resource_io_deadline_unsupported", http.StatusServiceUnavailable)
		return
	}
	defer ioOwner.Close()
	op, ok := parseResourceHTTPOperation(c.Request.Method, c.Request.URL.EscapedPath())
	if !ok {
		renderResourceError(c, "resource operation is not implemented", "unsupported_resource_operation", http.StatusNotImplemented)
		return
	}
	if op.topList {
		renderResourceError(c, "account-wide resource lists require an explicitly selected administrator channel", "unsupported_resource_list", http.StatusForbidden)
		return
	}
	operationCtx, cancel := context.WithTimeout(c.Request.Context(), resourceRequestTimeout)
	defer cancel()
	var owner *model.ResourceOwner
	var channelID int
	if op.id != "" {
		var err error
		owner, err = model.GetResourceOwner(operationCtx, op.kind, op.id, c.GetInt("id"))
		if err != nil {
			renderResourceOwnerError(c, err)
			return
		}
		channelID = owner.ChannelID
	}
	// Only conversation items contain resource references in this surface. The
	// shared extractor ignores function schemas and opaque tool/business values.
	if op.scanItems {
		body, err := io.ReadAll(io.LimitReader(c.Request.Body, resourceJSONBytes+1))
		if err != nil || int64(len(body)) > resourceJSONBytes {
			renderResourceError(c, "resource request exceeds the bounded JSON envelope", "resource_request_limit", http.StatusRequestEntityTooLarge)
			return
		}
		c.Request.Body.Close()
		c.Request.Body = io.NopCloser(bytes.NewReader(body))
		projection, err := resourceJSONProjection(body, c.Request.Header.Get("Content-Encoding"))
		if err != nil {
			renderResourceError(c, "cannot inspect resource reference envelope within local capacity", "unsupported_resource_envelope", http.StatusBadRequest)
			return
		}
		var fields map[string]any
		if err := json.Unmarshal(projection, &fields); err != nil {
			renderResourceError(c, "invalid JSON resource envelope", "invalid_request", http.StatusBadRequest)
			return
		}
		for _, ref := range responsescommon.InputResourceReferences(fields["items"]) {
			if ref.Kind != "file" && ref.Kind != "conversation" {
				renderResourceError(c, "referenced resource kind is not supported", "unsupported_resource_reference", http.StatusBadRequest)
				return
			}
			referenced, err := model.GetResourceOwner(operationCtx, ref.Kind, ref.ID, c.GetInt("id"))
			if err != nil {
				renderResourceOwnerError(c, err)
				return
			}
			if channelID != 0 && channelID != referenced.ChannelID {
				renderResourceError(c, "resource references must use the same channel", "resource_channel_conflict", http.StatusBadRequest)
				return
			}
			channelID = referenced.ChannelID
		}
	}
	if channelID == 0 {
		settingValue, _ := c.Get("token_setting")
		setting, _ := settingValue.(*model.TokenSetting)
		if setting != nil {
			channelID = setting.ResourceChannelID
		}
		if channelID <= 0 {
			renderResourceError(c, "resource_channel_id is required for resource creation without references", "resource_channel_required", http.StatusBadRequest)
			return
		}
	}
	channel, err := fetchOwnerChannelById(operationCtx, channelID)
	if err != nil {
		renderResourceError(c, "resource channel is unavailable", "resource_channel_unavailable", http.StatusServiceUnavailable)
		return
	}
	if op.rootCreate {
		if apiErr := authorizeResourceCreationChannel(c, channel); apiErr != nil {
			renderStoredResponsesError(c, apiErr)
			return
		}
	}
	provider, _, err := prepareProviderForChannel(c, "", channel)
	if err != nil {
		renderResourceError(c, "resource channel is unavailable", "resource_channel_unavailable", http.StatusServiceUnavailable)
		return
	}
	builder, ok := provider.(providersBase.ResourceRelayURLBuilder)
	if !ok {
		renderResourceError(c, "channel does not implement native resource relay", "unsupported_resource_channel", http.StatusBadRequest)
		return
	}
	upstreamURL, err := builder.BuildResourceRelayURL(c.Request.URL.EscapedPath(), c.Request.URL.RawQuery)
	if err != nil {
		renderResourceError(c, "resource endpoint is unavailable on this channel", "unsupported_resource_channel", http.StatusBadRequest)
		return
	}
	var reservation *model.ResourceOwner
	if op.createKind != "" {
		namespace, scope, err := model.ConservativeResponseOwnerIdentity(channel)
		if err != nil {
			renderResourceOwnerError(c, err)
			return
		}
		deadline, _ := operationCtx.Deadline()
		spec := model.ResourceOwnerReservation{Kind: op.createKind, UserID: c.GetInt("id"), TokenID: c.GetInt("token_id"), ChannelID: channelID, ProviderNamespace: namespace, ProviderScope: scope, SubmitDeadline: deadline}
		if owner != nil {
			spec.ParentID = &owner.ID
		}
		reservation, err = model.ReserveResourceOwner(operationCtx, spec)
		if err != nil {
			renderResourceOwnerError(c, err)
			return
		}
		defer func() {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 5*time.Second)
			defer cleanupCancel()
			// Bound owners are never removed by release. Ambiguous upstream execution
			// is not retried; an unbound reservation can be discarded safely.
			if err := model.ReleaseResourceOwnerReservation(cleanupCtx, reservation.ID, c.GetInt("id")); err != nil {
				logger.LogError(cleanupCtx, "resource reservation cleanup failed: "+err.Error())
			}
		}()
	}
	maxBytes := resourceJSONBytes
	if op.rootCreate && op.kind == "file" {
		maxBytes = resourceFileBytes
	}
	if op.uploadPart {
		maxBytes = resourcePartBytes
	}
	if c.Request.ContentLength > maxBytes {
		renderResourceError(c, "resource request exceeds local transfer capacity", "resource_request_limit", http.StatusRequestEntityTooLarge)
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
	defer c.Request.Body.Close()
	httpRequester := provider.GetRequester().ForHTTPProfile(requester.HTTPProfileLongStream)
	req, err := httpRequester.NewRequest(c.Request.Method, upstreamURL, httpRequester.WithContext(operationCtx), httpRequester.WithBody(c.Request.Body), httpRequester.WithHeader(provider.GetRequestHeaders()))
	if err != nil {
		renderResourceError(c, "cannot construct resource request", "invalid_request", http.StatusBadRequest)
		return
	}
	if c.Request.ContentLength >= 0 {
		req.ContentLength = c.Request.ContentLength
	}
	if err := requestctx.ApplyRegisteredExactWireRequestHeaders(req.Header, requestctx.NewHeaderSnapshot(c.Request.Header)); err != nil {
		renderResourceError(c, "invalid request header", "invalid_request", http.StatusBadRequest)
		return
	}
	response, apiErr := httpRequester.SendRequestRawNoRedirect(req)
	if apiErr != nil {
		renderStoredResponsesError(c, apiErr)
		return
	}
	if response == nil || response.Body == nil {
		renderResourceError(c, "empty upstream resource response", "resource_response_unavailable", http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	if response.StatusCode >= http.StatusBadRequest {
		apiErr := requester.HandleErrorResp(response, httpRequester.ErrorHandler, httpRequester.PrefixProviderErrors, true)
		renderStoredResponsesError(c, apiErr)
		return
	}
	if reservation != nil && response.StatusCode >= 200 && response.StatusCode < 300 {
		raw, err := io.ReadAll(io.LimitReader(response.Body, resourceJSONBytes+1))
		if err != nil || int64(len(raw)) > resourceJSONBytes {
			renderResourceError(c, "cannot observe resource identity within response capacity", "resource_owner_commit_failed", http.StatusBadGateway)
			return
		}
		projection, err := resourceJSONProjection(raw, response.Header.Get("Content-Encoding"))
		if err != nil {
			renderResourceError(c, "cannot inspect created resource identity within local capacity", "resource_owner_commit_failed", http.StatusBadGateway)
			return
		}
		id, err := createdResourceID(projection, op, response.Header.Get("Location"))
		if err != nil {
			renderResourceError(c, "cannot establish created resource ownership", "resource_owner_commit_failed", http.StatusBadGateway)
			return
		}
		// Client disconnect does not erase a resource that upstream already created.
		bindCtx, bindCancel := context.WithTimeout(context.WithoutCancel(operationCtx), 5*time.Second)
		_, err = model.BindResourceOwner(bindCtx, reservation.ID, c.GetInt("id"), id)
		bindCancel()
		if err != nil {
			renderResourceOwnerError(c, err)
			return
		}
		response.Body = io.NopCloser(bytes.NewReader(raw))
	}
	if op.contentDownload && owner != nil && response.StatusCode >= 200 && response.StatusCode < 300 {
		if apiErr := PrepareBatchFileDelivery(c, owner, response); apiErr != nil {
			renderStoredResponsesError(c, apiErr)
			return
		}
	}
	var deletion *resourceDeletionObserver
	if op.observeDelete && response.StatusCode >= 200 && response.StatusCode < 300 {
		deletion = &resourceDeletionObserver{ReadCloser: response.Body, encoding: response.Header.Get("Content-Encoding")}
		response.Body = deletion
	}
	response.Header = providerresponse.FilterCredentialHeaders(response.Header, requestctx.ProviderCredentials(c)...)
	response.Body = &resourceBoundedReadCloser{ReadCloser: response.Body, remaining: resourceDownloadBytes}
	if apiErr := responseMultipart(c, response, providerresponse.Policy{Operation: providerresponse.OperationRawRelay, DataPath: providerresponse.DataPathExactWire, BodyUnmodified: true, PreserveRedirect: true}); apiErr != nil {
		renderStoredResponsesError(c, apiErr)
		return
	}
	if deletion != nil && deletion.confirms(op.id) {
		observeCtx, observeCancel := context.WithTimeout(context.WithoutCancel(operationCtx), 5*time.Second)
		if err := model.ObserveResourceOwnerDelete(observeCtx, op.kind, op.id, c.GetInt("id")); err != nil {
			logger.LogError(observeCtx, "resource deletion observation failed: "+err.Error())
		}
		observeCancel()
	}
	recordZeroQuotaResponsesAudit(c, channelID, "resource lifecycle:"+op.kind)
}

// Inspect a bounded projection while retaining the client's encoded wire body.
func resourceJSONProjection(raw []byte, contentEncoding string) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(contentEncoding)) {
	case "", "identity":
		return raw, nil
	case "gzip":
		reader, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		defer reader.Close()
		decoded, err := io.ReadAll(io.LimitReader(reader, resourceJSONBytes+1))
		if err != nil || int64(len(decoded)) > resourceJSONBytes {
			return nil, fmt.Errorf("compressed resource envelope exceeds decoding capacity")
		}
		return decoded, nil
	default:
		return nil, fmt.Errorf("unsupported resource envelope encoding")
	}
}

func authorizeResourceCreationChannel(c *gin.Context, channel *model.Channel) *types.OpenAIErrorWithStatusCode {
	if apiErr := middleware.RefreshLongLivedPrincipal(c); apiErr != nil {
		return apiErr
	}
	if channel == nil || channel.Status != config.ChannelStatusEnabled || channel.DeletedAt.Valid {
		return common.StringErrorWrapperLocal("resource channel is disabled", "permission_denied", http.StatusForbidden)
	}
	group := groupctx.CurrentRoutingGroup(c)
	for _, allowed := range strings.Split(channel.Group, ",") {
		if strings.TrimSpace(allowed) == group && group != "" {
			return nil
		}
	}
	return common.StringErrorWrapperLocal("resource channel is not allowed for the current principal group", "permission_denied", http.StatusForbidden)
}

func renderResourceError(c *gin.Context, message, code string, status int) {
	renderStoredResponsesError(c, common.StringErrorWrapperLocal(message, code, status))
}
func renderResourceOwnerError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, model.ErrResourceOwnerNotFound):
		renderResourceError(c, "resource not found", "resource_not_found", http.StatusNotFound)
	case errors.Is(err, model.ErrResourceOwnerCapacity):
		renderResourceError(c, "resource ownership capacity exceeded", "resource_capacity_exceeded", http.StatusTooManyRequests)
	case errors.Is(err, model.ErrResourceOwnerConflict):
		renderResourceError(c, "resource ownership conflict", "resource_owner_conflict", http.StatusConflict)
	default:
		renderResourceError(c, "resource ownership store is unavailable", "resource_owner_unavailable", http.StatusServiceUnavailable)
	}
}

func parseResourceHTTPOperation(method, escapedPath string) (resourceHTTPOperation, bool) {
	var op resourceHTTPOperation
	parts := strings.Split(escapedPath, "/")
	if len(parts) < 3 || parts[0] != "" || parts[1] != "v1" {
		return op, false
	}
	switch parts[2] {
	case "files":
		op.kind = "file"
	case "uploads":
		op.kind = "upload"
	case "conversations":
		op.kind = "conversation"
	default:
		return parseStoredChatHTTPOperation(method, escapedPath)
	}
	for i := 3; i < len(parts); i++ {
		decoded, err := url.PathUnescape(parts[i])
		if err != nil || decoded == "" || decoded == "." || decoded == ".." || strings.ContainsAny(decoded, "/\\\x00") {
			return op, false
		}
		parts[i] = decoded
	}
	if len(parts) == 3 {
		if method == http.MethodGet {
			op.topList = true
			return op, true
		}
		if method == http.MethodPost {
			op.rootCreate = true
			op.createKind = op.kind
			op.scanItems = op.kind == "conversation"
			return op, true
		}
		return op, false
	}
	op.id = parts[3]
	switch op.kind {
	case "file":
		if len(parts) == 4 && (method == http.MethodGet || method == http.MethodDelete) {
			op.observeDelete = method == http.MethodDelete
			return op, true
		}
		op.contentDownload = len(parts) == 5 && parts[4] == "content" && method == http.MethodGet
		return op, op.contentDownload
	case "upload":
		if len(parts) != 5 || method != http.MethodPost {
			return op, false
		}
		switch parts[4] {
		case "parts":
			op.uploadPart = true
			return op, true
		case "complete":
			op.createKind = "file"
			return op, true
		case "cancel":
			return op, true
		}
	case "conversation":
		if len(parts) == 4 && (method == http.MethodGet || method == http.MethodPost || method == http.MethodDelete) {
			op.observeDelete = method == http.MethodDelete
			return op, true
		}
		if len(parts) == 5 && parts[4] == "items" && (method == http.MethodGet || method == http.MethodPost) {
			op.scanItems = method == http.MethodPost
			return op, true
		}
		if len(parts) == 6 && parts[4] == "items" && (method == http.MethodGet || method == http.MethodDelete) {
			return op, true
		}
	}
	return op, false
}

func createdResourceID(raw []byte, op resourceHTTPOperation, location string) (string, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return "", err
	}
	if op.kind == "upload" && op.createKind == "file" {
		var file map[string]json.RawMessage
		if err := json.Unmarshal(fields["file"], &file); err != nil {
			return "", err
		}
		fields = file
	}
	var id string
	if json.Unmarshal(fields["id"], &id) != nil || strings.TrimSpace(id) == "" {
		return "", fmt.Errorf("created resource has no identity")
	}
	if location != "" {
		parsed, err := url.Parse(location)
		if err != nil {
			return "", err
		}
		root := "/v1/" + op.createKind + "s/"
		if strings.HasPrefix(parsed.Path, root) && strings.TrimPrefix(parsed.Path, root) != id {
			return "", fmt.Errorf("conflicting response resource identity")
		}
	}
	return id, nil
}

type resourceBoundedReadCloser struct {
	io.ReadCloser
	remaining int64
}

func (r *resourceBoundedReadCloser) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		var probe [1]byte
		n, err := r.ReadCloser.Read(probe[:])
		if n > 0 {
			return 0, fmt.Errorf("resource response exceeds local transfer capacity")
		}
		return 0, err
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	n, err := r.ReadCloser.Read(p)
	r.remaining -= int64(n)
	return n, err
}

// Deletion metadata is optional observation: an unfamiliar or large reply still
// streams unchanged and does not revoke the durable authorization fact.
type resourceDeletionObserver struct {
	io.ReadCloser
	body     []byte
	overflow bool
	encoding string
}

func (r *resourceDeletionObserver) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if !r.overflow {
		if len(r.body)+n > 64<<10 {
			r.overflow = true
			r.body = nil
		} else {
			r.body = append(r.body, p[:n]...)
		}
	}
	return n, err
}
func (r *resourceDeletionObserver) confirms(id string) bool {
	if r.overflow {
		return false
	}
	var observation struct {
		ID      string `json:"id"`
		Deleted bool   `json:"deleted"`
	}
	projection, err := resourceJSONProjection(r.body, r.encoding)
	return err == nil && json.Unmarshal(projection, &observation) == nil && observation.ID == id && observation.Deleted
}
