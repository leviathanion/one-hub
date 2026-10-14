package providers

import (
	"encoding/json"
	"errors"
	"testing"

	"one-api/common/config"
	"one-api/model"
	"one-api/providers/antigravity/wire"
	"one-api/providers/base"
	"one-api/types"
)

type nativeCrossProtocolFactory struct{ assessCalls *int }

func (nativeCrossProtocolFactory) Create(*model.Channel) base.ProviderInterface {
	panic("assessment must not create a provider")
}

func TestResponsesExtensionAssessmentUsesEffectivePreAddBody(t *testing.T) {
	carrier, err := wire.EncodeCarrier([]wire.CarrierPart{{Index: 0, Kind: "signature", Signature: json.RawMessage(`"opaque"`)}})
	if err != nil {
		t.Fatal(err)
	}
	item, _ := json.Marshal([]map[string]interface{}{{"type": "reasoning", "summary": []any{}, "encrypted_content": carrier}})
	fields := map[string]json.RawMessage{"model": json.RawMessage(`"model"`), "store": json.RawMessage(`false`), "input": json.RawMessage(`"hello"`)}
	addition, _ := json.Marshal(map[string]interface{}{"pre_add": true, "overwrite": true, "input": json.RawMessage(item)})
	additionText := string(addition)
	channel := &model.Channel{Type: config.ChannelTypeOpenAI, CustomParameter: &additionText}
	if err := ValidateResponsesRequest(channel, base.OperationResponsesCreate, fields, "model", true); err == nil {
		t.Fatal("pre_add 引入的代理状态被发送给不支持的候选")
	}
	if err := ValidateResponsesRequest(channel, base.OperationResponsesCreate, fields, "model", false); err != nil {
		t.Fatalf("已完成 pre_add 的调用又应用了配置：%v", err)
	}
	removal := `{"pre_add":true,"remove_params":["input"]}`
	channel.CustomParameter = &removal
	fields["input"] = item
	if err := ValidateResponsesRequest(channel, base.OperationResponsesCreate, fields, "model", true); err != nil {
		t.Fatalf("pre_add 已移除的代理状态仍限制候选：%v", err)
	}
}
func (nativeCrossProtocolFactory) ResponsesSupport(*model.Channel) base.OperationSupport {
	return base.OperationSupport{Operations: map[base.Operation]base.DataPath{base.OperationResponsesCreate: base.DataPathCrossProtocol}}
}
func (nativeCrossProtocolFactory) ValidateResponsesRequest(*model.Channel, base.Operation, map[string]json.RawMessage, string, bool) error {
	return nil
}
func (f nativeCrossProtocolFactory) AssessChatRequest(*model.Channel, string, *types.ChatCompletionRequest) (base.ChatRequestSupport, error) {
	*f.assessCalls++
	return base.ChatRequestSupport{}, nil
}
func (nativeCrossProtocolFactory) AssessRequestExtensions(candidate *model.Channel, _ base.RequestProtocol, fields map[string]json.RawMessage) error {
	if _, owned := fields["proxy_test_state"]; owned && candidate.Type != -1901 {
		return &base.RequestCapabilityError{Param: "proxy_test_state", Message: "selected adapter cannot consume this proxy state"}
	}
	return nil
}

func TestResponsesTargetDistinguishesProviderCrossProtocolFromChatFallback(t *testing.T) {
	calls := 0
	providerFactories[-1901] = nativeCrossProtocolFactory{assessCalls: &calls}
	t.Cleanup(func() { delete(providerFactories, -1901) })
	for _, enabled := range []bool{false, true} {
		support := ResolveAdapterSupport(&model.Channel{Type: -1901, CompatibleResponse: enabled})
		if support.ResponsesCreateTarget != base.ResponsesCreateTargetProvider {
			t.Fatalf("declared cross-protocol provider became Chat fallback: %+v", support)
		}
	}
	support := ResolveAdapterSupport(&model.Channel{Type: config.ChannelTypeAnthropic, CompatibleResponse: true})
	if support.ResponsesCreateTarget != base.ResponsesCreateTargetChatAdapter {
		t.Fatalf("missing explicit Chat fallback: %+v", support)
	}
}

func TestRequestExtensionOwnerRejectsForeignCandidateBeforeCandidateAssessment(t *testing.T) {
	calls := 0
	providerFactories[-1901] = nativeCrossProtocolFactory{assessCalls: &calls}
	providerFactories[-1902] = nativeCrossProtocolFactory{assessCalls: &calls}
	t.Cleanup(func() { delete(providerFactories, -1901); delete(providerFactories, -1902) })
	fields := map[string]json.RawMessage{"proxy_test_state": json.RawMessage(`"opaque"`)}
	_, err := AssessChatRequest(&model.Channel{Type: -1902}, "model", &types.ChatCompletionRequest{}, fields)
	var capabilityErr *base.RequestCapabilityError
	if !errors.As(err, &capabilityErr) || capabilityErr.Param != "proxy_test_state" || calls != 0 {
		t.Fatalf("extension owner did not fence foreign candidate: calls=%d err=%v", calls, err)
	}
	if _, err := AssessChatRequest(&model.Channel{Type: -1901}, "model", &types.ChatCompletionRequest{}, fields); err != nil || calls != 1 {
		t.Fatalf("own adapter rejected extension: calls=%d err=%v", calls, err)
	}
}
