package wire

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"one-api/common/jsonobject"
)

type Dialect string

const (
	DialectGemini    Dialect = "gemini"
	DialectClaude    Dialect = "claude"
	DialectChat      Dialect = "chat"
	DialectResponses Dialect = "responses"
)

type Context struct {
	Dialect            Dialect
	Model              string
	ToolNames          map[string]string
	Stream             bool
	ExpectedCandidates int
	IncludeUsage       bool
}

type Request struct {
	Body    json.RawMessage
	Context *Context
}

// ConvertRequest 是评估和执行共用的纯转换；不读取凭据或调用上游。
func ConvertRequest(dialect Dialect, raw []byte, model string) (*Request, error) {
	fields, err := object(raw, "request")
	if err != nil {
		return nil, err
	}
	ctx := &Context{Dialect: dialect, Model: model, ToolNames: map[string]string{}, Stream: isTrue(fields["stream"]), ExpectedCandidates: 1}
	if dialect == DialectGemini {
		// 原生字段只用于观察；未知类型与未来联合类型仍交给上游。
		observeCandidates(fields, ctx)
		return &Request{Body: append(json.RawMessage(nil), raw...), Context: ctx}, nil
	}
	if len(raw) > MaxRetainedBytes {
		return nil, unsupported("request", "请求超过转换容量")
	}
	var body json.RawMessage
	switch dialect {
	case DialectChat:
		body, err = convertChat(fields, ctx)
	case DialectClaude:
		body, err = convertClaude(fields, ctx)
	case DialectResponses:
		body, err = convertResponses(fields, ctx)
	default:
		err = unsupported("request", "不支持的请求协议")
	}
	if err != nil {
		return nil, err
	}
	if !json.Valid(body) {
		return nil, unsupported("request", "目标请求构造失败")
	}
	retained := len(raw) + len(body)
	for k, v := range ctx.ToolNames {
		retained += len(k) + len(v)
	}
	if retained > MaxRetainedBytes {
		return nil, unsupported("request", "转换结果超过保留容量")
	}
	return &Request{Body: body, Context: ctx}, nil
}

// FinalizeRequest 读取应用渠道参数后的最终请求，只检查对客 codec 无法表达的
// 响应语义。它与转换一样无 I/O，原生路径仍由上游解释参数和未来字段。
func FinalizeRequest(request *Request) error {
	if request == nil || request.Context == nil {
		return unsupported("request", "缺少请求转换上下文")
	}
	body, err := object(request.Body, "request")
	if err != nil {
		return err
	}
	observeCandidates(body, request.Context)
	if request.Context.Dialect == DialectGemini {
		return nil
	}
	if len(request.Body) > MaxRetainedBytes {
		return unsupported("request", "最终请求超过转换容量")
	}
	if request.Context.ExpectedCandidates > 1 && (request.Context.Dialect == DialectClaude || request.Context.Dialect == DialectResponses) {
		return unsupported("generationConfig.candidateCount", "该响应协议不能表示多个候选")
	}
	for _, configKey := range []string{"generationConfig", "generation_config"} {
		var cfg fields
		if json.Unmarshal(body[configKey], &cfg) != nil {
			continue
		}
		for _, key := range []string{"responseModalities", "response_modalities"} {
			if value, ok := cfg[key]; ok {
				var modalities []string
				if json.Unmarshal(value, &modalities) != nil {
					return unsupported(configKey+"."+key, "无法确定目标响应媒体的表示")
				}
				for _, modality := range modalities {
					if modality != "TEXT" {
						return unsupported(configKey+"."+key, "当前跨协议响应仅支持文本与工具")
					}
				}
			}
		}
		for _, key := range []string{"responseLogprobs", "response_logprobs"} {
			if isTrue(cfg[key]) {
				return unsupported(configKey+"."+key, "当前响应转换不能保留 logprobs")
			}
		}
		var logprobs json.Number
		if json.Unmarshal(cfg["logprobs"], &logprobs) == nil {
			if value, err := logprobs.Float64(); err == nil && value != 0 {
				return unsupported(configKey+".logprobs", "当前响应转换不能保留 logprobs")
			}
		}
	}
	if tools, ok := body["tools"]; ok && !null(tools) {
		items, err := array(tools, "tools")
		if err != nil {
			return err
		}
		for i, item := range items {
			path := fmt.Sprintf("tools[%d]", i)
			tool, err := object(item, path)
			if err != nil {
				return err
			}
			for key := range tool {
				if key != "functionDeclarations" && key != "function_declarations" {
					return unsupported(path+"."+key, "当前跨协议响应仅支持客户端 function tools")
				}
			}
		}
	}
	return nil
}

func observeCandidates(body fields, ctx *Context) {
	ctx.ExpectedCandidates = 1
	for _, configKey := range []string{"generationConfig", "generation_config"} {
		var cfg fields
		if json.Unmarshal(body[configKey], &cfg) != nil {
			continue
		}
		for _, countKey := range []string{"candidateCount", "candidate_count"} {
			var count int
			if json.Unmarshal(cfg[countKey], &count) == nil && count > 0 {
				ctx.ExpectedCandidates = count
			}
		}
	}
}

type fields = map[string]json.RawMessage

func object(raw []byte, path string) (fields, error) {
	parsed, err := jsonobject.Parse(raw)
	if err != nil {
		return nil, invalid(path, err)
	}
	return parsed.Fields, nil
}

func array(raw []byte, path string) ([]json.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '[' {
		return nil, unsupported(path, "转换需要数组")
	}
	var result []json.RawMessage
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, invalid(path, err)
	}
	return result, nil
}

func stringValue(raw []byte) string {
	var result string
	_ = json.Unmarshal(raw, &result)
	return result
}
func isTrue(raw []byte) bool  { return bytes.Equal(bytes.TrimSpace(raw), []byte("true")) }
func isFalse(raw []byte) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("false")) }
func null(raw []byte) bool    { return len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }
func marshal(value any) json.RawMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		// 仅接受已验证 JSON 与内部构造的标量；异常标记使外层编码继续
		// 失败，由 ConvertRequest 统一返回错误，不生成部分请求或 panic。
		return json.RawMessage("!")
	}
	return raw
}

func textValue(raw []byte, path string) (string, error) {
	var value string
	if null(raw) || json.Unmarshal(raw, &value) != nil {
		return "", unsupported(path, "转换需要字符串")
	}
	return value, nil
}

func checkFields(source fields, path string, allowed ...string) error {
	set := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		set[name] = true
	}
	first := ""
	found := false
	for name := range source {
		if !set[name] && (!found || name < first) {
			first, found = name, true
		}
	}
	if found {
		return unsupported(path+"."+first, "当前转换尚未支持该字段")
	}
	return nil
}

// Claude 的可空 cache_control 只在声明了该字段的内容块/工具上调用。
// null 没有缓存断点；ephemeral 即使省略 ttl 也仍要求默认期限和计费语义。
func claudeCacheControl(raw json.RawMessage, path string) error {
	if null(raw) {
		return nil
	}
	cache, err := object(raw, path)
	if err != nil {
		return err
	}
	if err = checkFields(cache, path, "type", "ttl"); err != nil {
		return err
	}
	if stringValue(cache["type"]) != "ephemeral" {
		return unsupported(path+".type", "当前转换仅识别 ephemeral 缓存声明，不能解释此缓存类型")
	}
	if ttl, ok := cache["ttl"]; ok {
		if value := stringValue(ttl); value != "5m" && value != "1h" {
			return unsupported(path+".ttl", "当前转换不能表示该缓存期限")
		}
		return unsupported(path+".ttl", "Antigravity 尚无保留显式缓存期限及缓存计费的映射")
	}
	return unsupported(path, "Antigravity 尚无保留缓存断点、默认 5m 期限及缓存计费的映射")
}

func claudeEagerInputStreaming(raw json.RawMessage, path string) error {
	if isFalse(raw) {
		// Claude codec 在完整参数和关联签名就绪后才交付 tool_use。
		return nil
	}
	if null(raw) {
		return unsupported(path, "null 的交付方式依赖 beta headers；当前转换仅支持显式 false 的完整工具参数交付")
	}
	if isTrue(raw) {
		return unsupported(path, "当前转换需缓冲完整工具参数及迟到签名，不能提供生成时的 eager 输入流；可使用 false")
	}
	return unsupported(path, "工具输入流控制必须是布尔值或 null")
}

func content(role string, parts []json.RawMessage) json.RawMessage {
	if parts == nil {
		parts = []json.RawMessage{}
	}
	return marshal(map[string]any{"role": role, "parts": parts})
}

// 连续工具结果组成同一轮；遇到其他输入时结束，保留普通消息的边界。
type toolResultTurn struct {
	parts []json.RawMessage
}

func (turn *toolResultTurn) flush(contents []json.RawMessage) []json.RawMessage {
	if len(turn.parts) > 0 {
		contents = append(contents, content("user", turn.parts))
		turn.parts = nil
	}
	return contents
}

func textPart(raw json.RawMessage) json.RawMessage { return marshal(fields{"text": raw}) }
func addRaw(dst fields, key string, src fields, name string) {
	if value, exists := src[name]; exists {
		dst[key] = value
	}
}

func registerCall(ctx *Context, id, name, path string) error {
	if id == "" || name == "" {
		return unsupported(path, "工具调用必须有可关联的 ID 与名称")
	}
	if existing, ok := ctx.ToolNames[id]; ok && existing != name {
		return unsupported(path, "同一工具调用 ID 对应多个名称")
	}
	ctx.ToolNames[id] = name
	return nil
}

func functionPart(id, name string, args json.RawMessage, path string) (json.RawMessage, error) {
	if _, err := object(args, path); err != nil {
		return nil, err
	}
	return marshal(map[string]any{"functionCall": map[string]any{"id": id, "name": name, "args": args}}), nil
}

func dataImage(url, path string) (json.RawMessage, error) {
	if !strings.HasPrefix(url, "data:") {
		return remoteImage(url, path)
	}
	head, data, ok := strings.Cut(url[5:], ",")
	if !ok || !strings.HasSuffix(head, ";base64") {
		return nil, unsupported(path, "需要 base64 data URL")
	}
	mime := strings.TrimSuffix(head, ";base64")
	if !strings.HasPrefix(mime, "image/") {
		return nil, unsupported(path, "媒体不是内联图像")
	}
	if _, err := base64.StdEncoding.DecodeString(data); err != nil {
		return nil, invalid(path, err)
	}
	return marshal(map[string]any{"inlineData": map[string]string{"mimeType": mime, "data": data}}), nil
}

// 地址权限和下载属于共享媒体准备；纯转换只保留 URI 表示，不自行访问资源。
func remoteImage(value, path string) (json.RawMessage, error) {
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, unsupported(path, "无法表示该图像 URI")
	}
	return marshal(map[string]any{"fileData": map[string]string{"fileUri": value}}), nil
}

func toolDeclarations(raw json.RawMessage, dialect Dialect, path string) (json.RawMessage, error) {
	items, err := array(raw, path)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return json.RawMessage("[]"), nil
	}
	declarations := make([]json.RawMessage, 0, len(items))
	for i, item := range items {
		p := fmt.Sprintf("%s[%d]", path, i)
		o, err := object(item, p)
		if err != nil {
			return nil, err
		}
		var f fields
		switch dialect {
		case DialectChat:
			if err = checkFields(o, p, "type", "function"); err != nil {
				return nil, err
			}
			if stringValue(o["type"]) != "function" {
				return nil, unsupported(p+".type", "仅支持 function tools")
			}
			f, err = object(o["function"], p+".function")
		case DialectClaude:
			if rawType, ok := o["type"]; ok && stringValue(rawType) != "custom" {
				return nil, unsupported(p+".type", "仅支持客户端 function tools")
			}
			f = o
		case DialectResponses:
			if stringValue(o["type"]) != "function" {
				return nil, unsupported(p+".type", "仅支持 function tools")
			}
			f = o
		}
		if err != nil {
			return nil, err
		}
		allowed := []string{"type", "name", "description", "parameters", "input_schema", "strict"}
		if dialect == DialectClaude {
			allowed = append(allowed, "cache_control", "eager_input_streaming")
		}
		if err = checkFields(f, p, allowed...); err != nil {
			return nil, err
		}
		if dialect == DialectClaude {
			if err = claudeCacheControl(f["cache_control"], p+".cache_control"); err != nil {
				return nil, err
			}
			if eager, ok := f["eager_input_streaming"]; ok {
				if err = claudeEagerInputStreaming(eager, p+".eager_input_streaming"); err != nil {
					return nil, err
				}
			}
		}
		if strict, ok := f["strict"]; ok && !isFalse(strict) {
			return nil, unsupported(p+".strict", "目标协议不能保证 strict 约束")
		}
		decl := fields{}
		addRaw(decl, "name", f, "name")
		addRaw(decl, "description", f, "description")
		if dialect == DialectClaude {
			addRaw(decl, "parametersJsonSchema", f, "input_schema")
		} else {
			addRaw(decl, "parametersJsonSchema", f, "parameters")
		}
		// schema 保持 raw；不展开引用、不改变 union，也不校验实例。
		declarations = append(declarations, marshal(decl))
	}
	return marshal([]any{map[string]any{"functionDeclarations": declarations}}), nil
}

func toolChoice(raw json.RawMessage, dialect Dialect, path string) (json.RawMessage, error) {
	mode := ""
	name := ""
	if value, err := textValue(raw, path); err == nil {
		switch value {
		case "auto":
			mode = "AUTO"
		case "none":
			mode = "NONE"
		case "required":
			mode = "ANY"
		default:
			return nil, unsupported(path, "无法映射工具选择")
		}
	} else {
		o, err := object(raw, path)
		if err != nil {
			return nil, err
		}
		if dialect == DialectClaude {
			if err = checkFields(o, path, "type", "name", "disable_parallel_tool_use"); err != nil {
				return nil, err
			}
			if isTrue(o["disable_parallel_tool_use"]) {
				return nil, unsupported(path+".disable_parallel_tool_use", "无法保证单个工具调用")
			}
			if value, ok := o["disable_parallel_tool_use"]; ok && !isFalse(value) {
				return nil, unsupported(path+".disable_parallel_tool_use", "并行工具控制必须是布尔值")
			}
			switch stringValue(o["type"]) {
			case "auto":
				mode = "AUTO"
			case "none":
				mode = "NONE"
			case "any":
				mode = "ANY"
			case "tool":
				mode = "ANY"
				name = stringValue(o["name"])
				if name == "" {
					return nil, unsupported(path+".name", "缺少工具名称")
				}
			default:
				return nil, unsupported(path, "无法映射工具选择")
			}
		} else {
			if err = checkFields(o, path, "type", "name", "function"); err != nil {
				return nil, err
			}
			if stringValue(o["type"]) != "function" {
				return nil, unsupported(path, "仅支持 function 选择")
			}
			name = stringValue(o["name"])
			if dialect == DialectChat {
				f, err := object(o["function"], path+".function")
				if err != nil {
					return nil, err
				}
				if err = checkFields(f, path+".function", "name"); err != nil {
					return nil, err
				}
				name = stringValue(f["name"])
			}
			if name == "" {
				return nil, unsupported(path, "缺少工具名称")
			}
			mode = "ANY"
		}
	}
	config := map[string]any{"mode": mode}
	if name != "" {
		config["allowedFunctionNames"] = []string{name}
	}
	return marshal(map[string]any{"functionCallingConfig": config}), nil
}

func thinkingEffort(raw json.RawMessage, path string) (fields, error) {
	effort, err := textValue(raw, path)
	if err != nil {
		return nil, err
	}
	if effort == "none" {
		return fields{"thinkingBudget": json.RawMessage("0")}, nil
	}
	if effort == "low" || effort == "medium" || effort == "high" || effort == "minimal" {
		return fields{"thinkingLevel": marshal(strings.ToUpper(effort))}, nil
	}
	return nil, unsupported(path, "该思考强度没有明确等价映射")
}

func responseFormat(raw json.RawMessage, path string) (fields, error) {
	o, err := object(raw, path)
	if err != nil {
		return nil, err
	}
	if err = checkFields(o, path, "type", "json_schema", "name", "description", "schema", "strict"); err != nil {
		return nil, err
	}
	switch stringValue(o["type"]) {
	case "text":
		return fields{"responseMimeType": marshal("text/plain")}, nil
	case "json_object":
		return fields{"responseMimeType": marshal("application/json")}, nil
	case "json_schema":
		schema := o
		if nested, ok := o["json_schema"]; ok {
			schema, err = object(nested, path+".json_schema")
			if err != nil {
				return nil, err
			}
			if err = checkFields(schema, path+".json_schema", "name", "description", "schema", "strict"); err != nil {
				return nil, err
			}
		}
		if strict, ok := schema["strict"]; ok && !isFalse(strict) {
			return nil, unsupported(path+".strict", "目标不能保证 strict 解码语义")
		}
		if _, ok := schema["schema"]; !ok {
			return nil, unsupported(path+".schema", "缺少目标 schema")
		}
		return fields{"responseMimeType": marshal("application/json"), "responseJsonSchema": schema["schema"]}, nil
	default:
		return nil, unsupported(path+".type", "该响应格式没有等价映射")
	}
}
