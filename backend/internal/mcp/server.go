package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"papafeiji/backend/pkg/timeutil"
)

const (
	mcpProtocolVersion = "2024-11-05"
	mcpServerName      = "memory-mcp"
	mcpServerVersion   = "1.0.0"
)

// mcpMinProtocolVersion 是 initialize 可接受的最低协议版本。
// 由于 Streamable HTTP 在 2024-11-05 定型，后续版本主要补充能力而非改变核心
// JSON-RPC 格式，因此接受该日期及之后的所有版本并回显客户端版本，以获得更好
// 的客户端兼容性；空版本仍按默认值通过。
const mcpMinProtocolVersion = "2024-11-05"

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type jsonRPCResponse struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      interface{}   `json:"id,omitempty"`
	Result  interface{}   `json:"result,omitempty"`
	Error   *jsonRPCError `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func jsonRPCErrorResponse(id interface{}, code int, message string) *jsonRPCResponse {
	return &jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &jsonRPCError{Code: code, Message: message},
	}
}

func extractBearer(header string) string {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return ""
	}
	return strings.TrimPrefix(header, prefix)
}

type apiKeyInfo struct {
	userID string
	apiKey string
}

func (h *Handler) authenticateAPIKey(ctx context.Context, rawKey string) (apiKeyInfo, error) {
	if rawKey == "" {
		return apiKeyInfo{}, fmt.Errorf("missing api key")
	}
	key, err := h.pool.Queries().GetAPIKeyByHash(ctx, hashKey(rawKey))
	if err != nil || key.ApiKey == "" {
		return apiKeyInfo{}, fmt.Errorf("invalid api key")
	}
	return apiKeyInfo{
		userID: key.UserID,
		apiKey: key.ApiKey,
	}, nil
}

// serveStreamable 是 MCP Streamable HTTP 请求的处理链（单一 POST 端点，无状态同步响应）。
// SaaS 模式下由 Cloudflare Worker 内部端点 /internal/mcp/rpc 调用（RegisterInternal）；
// 开源版下由公开 /mcp 路由直接调用（RegisterPublic）。两种模式的 API Key 鉴权均在本函数内完成。
func (h *Handler) serveStreamable(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	auth := extractBearer(r.Header.Get("Authorization"))
	if auth == "" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	if !h.rateLimiter.Allow(r) {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}

	// 请求级错误按 4xx/5xx 返回；业务级 JSON-RPC 错误由 processMessage 统一以 200+JSON 返回。
	// 认证成功后才读取 body，不消耗未认证请求的带宽。
	key, err := h.authenticateAPIKey(ctx, auth)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxMcpMessageSize+1))
	if err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if int64(len(body)) > maxMcpMessageSize {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}

	var req jsonRPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}

	resp := h.processMessage(ctx, key.userID, req)
	if resp == nil {
		// 通知类请求（无 id）：成功接受但不返回 JSON-RPC 响应体。
		w.WriteHeader(http.StatusAccepted)
		return
	}

	data, err := json.Marshal(resp)
	if err != nil {
		slog.ErrorContext(ctx, "mcp marshal response failed", slog.Any("error", err))
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	if len(data) > maxMcpMessageSize {
		errResp := jsonRPCErrorResponse(req.ID, -32603, "response too large")
		var err error
		data, err = json.Marshal(errResp)
		if err != nil {
			slog.ErrorContext(ctx, "mcp marshal error response failed", slog.Any("error", err))
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(data); err != nil {
		slog.ErrorContext(ctx, "mcp write response failed", slog.Any("error", err))
	}
}

func (h *Handler) processMessage(ctx context.Context, userID string, req jsonRPCRequest) *jsonRPCResponse {
	switch req.Method {
	case "initialize":
		var initReq struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if err := json.Unmarshal(req.Params, &initReq); err != nil {
			slog.WarnContext(ctx, "mcp initialize params unmarshal failed", slog.Any("error", err))
		}
		version := initReq.ProtocolVersion
		if version == "" {
			version = mcpProtocolVersion
		} else if version < mcpMinProtocolVersion {
			return jsonRPCErrorResponse(req.ID, -32602, "unsupported protocol version")
		}
		return &jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]interface{}{
				"protocolVersion": version,
				"capabilities": map[string]interface{}{
					"tools":   map[string]interface{}{},
					"prompts": map[string]interface{}{},
				},
				"serverInfo": map[string]string{
					"name":    mcpServerName,
					"version": mcpServerVersion,
				},
			},
		}

	case "notifications/initialized":
		return nil

	case "tools/list":
		return &jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]interface{}{
				"tools": buildToolsList(),
			},
		}

	case "tools/call":
		return h.handleToolCall(ctx, userID, req)

	case "prompts/list":
		return h.handlePromptsList(req)

	case "prompts/get":
		return h.handlePromptsGet(req)

	default:
		if req.ID == nil {
			return nil
		}
		return jsonRPCErrorResponse(req.ID, -32601, "method not found")
	}
}

func buildToolsList() []map[string]interface{} {
	return []map[string]interface{}{
		{
			"name":        "store_memory",
			"description": "保存一条记录到记忆库，可供日后查询。",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"record_time": map[string]string{
						"type":        "string",
						"description": "记录发生的时间，ISO 8601 格式，如 2025-07-10T14:30:00+08:00",
					},
					"title": map[string]string{
						"type":        "string",
						"description": "标题，最多 50 个字",
					},
					"content": map[string]string{
						"type":        "string",
						"description": "正文内容，最多 10000 个字",
					},
				},
				"required": []string{"record_time", "title", "content"},
			},
		},
		{
			"name":        "query_memories",
			"description": "查询记忆库，返回手动保存的记忆和应用内自动记录的日记，按时间倒序排列。不传参数时默认最近 90 天。",
			"inputSchema": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"start_date": map[string]string{
						"type":        "string",
						"description": "起始日期，YYYY-MM-DD 格式，如 2025-07-01。不传则从 90 天前开始",
					},
					"end_date": map[string]string{
						"type":        "string",
						"description": "截止日期，YYYY-MM-DD 格式，如 2025-07-10。不传则截至今天",
					},
					"limit": map[string]string{
						"type":        "integer",
						"description": "返回条数上限，默认 500，最大 1000",
					},
				},
			},
		},
	}
}

func buildPromptsList() []map[string]interface{} {
	return []map[string]interface{}{
		{
			"name":        "memory-sync",
			"description": "将AI助手本地生成的记忆记录上报到记忆助手，支持次日凌晨上报、本地缓存浏览和断网兜底查询",
			"arguments":   []map[string]interface{}{},
		},
		{
			"name":        "memory-digest",
			"description": "汇总用户当天的聊天记录或外部转发文本，生成结构化的记忆记录。同一天可生成多条，每次触发独立生成",
			"arguments":   []map[string]interface{}{},
		},
	}
}

func (h *Handler) handleToolCall(ctx context.Context, userID string, req jsonRPCRequest) *jsonRPCResponse {
	if req.ID == nil {
		return nil
	}
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return jsonRPCErrorResponse(req.ID, -32602, "invalid params")
	}

	switch params.Name {
	case "store_memory":
		return h.handleStoreMemoryTool(ctx, userID, req.ID, params.Arguments)
	case "query_memories":
		return h.handleListMemoriesTool(ctx, userID, req.ID, params.Arguments)
	default:
		return jsonRPCErrorResponse(req.ID, -32602, "unknown tool")
	}
}

func (h *Handler) handleStoreMemoryTool(ctx context.Context, userID string, id interface{}, args json.RawMessage) *jsonRPCResponse {
	req, err := parseStoreMemoryArgs(args)
	if err != nil {
		return jsonRPCErrorResponse(id, -32602, err.Error())
	}

	memoryID, err := h.createMemoryForUser(ctx, userID, req)
	if err != nil {
		return jsonRPCErrorResponse(id, -32603, "保存失败，请重试")
	}

	return &jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result: map[string]interface{}{
			"content": []map[string]string{
				{"type": "text", "text": "已保存。ID: " + memoryID},
			},
		},
	}
}

func (h *Handler) handleListMemoriesTool(ctx context.Context, userID string, id interface{}, args json.RawMessage) *jsonRPCResponse {
	startDate, endDate, limit, err := parseToolArgs(args)
	if err != nil {
		return jsonRPCErrorResponse(id, -32602, err.Error())
	}

	items, err := h.queryMemoryItems(ctx, userID, startDate, endDate, limit)
	if err != nil {
		return jsonRPCErrorResponse(id, -32603, "查询失败，请重试")
	}
	// R3：与 REST 端点（GetDiary/ListMemories）保持同一字节预算截断策略。
	// 截断时在正文末尾附加明确提示，并在结果对象上带结构化 truncated 标记——
	// MCP 最佳实践：让模型与客户端都能感知截断，提示用户缩小日期范围/limit，
	// 而不是整包失败 500 或静默返回不完整数据。
	items, truncated := truncateMemoryItems(items)
	text := formatMemoryText(items)
	if truncated {
		text += "\n\n[注意] 结果数量过多，以上仅返回最近的 " + strconv.Itoa(len(items)) +
			" 条。请缩小 start_date/end_date 范围或减小 limit 参数以获取完整结果。"
	}
	return &jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result: map[string]interface{}{
			"content": []map[string]string{
				{"type": "text", "text": text},
			},
			"truncated": truncated,
		},
	}
}

func (h *Handler) handlePromptsList(req jsonRPCRequest) *jsonRPCResponse {
	if req.ID == nil {
		return nil
	}
	return &jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result: map[string]interface{}{
			"prompts": buildPromptsList(),
		},
	}
}

func (h *Handler) handlePromptsGet(req jsonRPCRequest) *jsonRPCResponse {
	if req.ID == nil {
		return nil
	}
	var params struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return jsonRPCErrorResponse(req.ID, -32602, "prompt not found")
	}
	var desc, text string
	switch params.Name {
	case "memory-sync":
		desc = "将AI助手本地生成的记忆记录上报到记忆助手"
		text = memorySyncPrompt
	case "memory-digest":
		desc = "汇总用户当天的聊天记录或外部转发文本，生成结构化的记忆草稿并存储本地"
		text = memoryDigestPrompt
	default:
		return jsonRPCErrorResponse(req.ID, -32602, "prompt not found")
	}
	return &jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result: map[string]interface{}{
			"description": desc,
			"messages": []map[string]interface{}{
				{
					"role": "user",
					"content": map[string]string{
						"type": "text",
						"text": text,
					},
				},
			},
		},
	}
}

func parseToolArgs(args json.RawMessage) (time.Time, time.Time, int, error) {
	now := timeutil.NowShanghai()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, timeutil.Shanghai)
	endDate := today.Add(24*time.Hour - time.Nanosecond)
	startDate := today.AddDate(0, 0, -mcpDiaryDefaultDaysBack)
	limit := defaultMcpPageSize

	if len(args) == 0 {
		return startDate, endDate, limit, nil
	}

	var m map[string]interface{}
	if err := json.Unmarshal(args, &m); err != nil {
		return time.Time{}, time.Time{}, 0, fmt.Errorf("参数格式不正确")
	}

	if v, ok := m["start_date"].(string); ok && v != "" {
		parsed, err := time.ParseInLocation("2006-01-02", v, timeutil.Shanghai)
		if err != nil {
			return time.Time{}, time.Time{}, 0, fmt.Errorf("start_date 格式不正确，应为 YYYY-MM-DD")
		}
		startDate = parsed
	}
	if v, ok := m["end_date"].(string); ok && v != "" {
		parsed, err := time.ParseInLocation("2006-01-02", v, timeutil.Shanghai)
		if err != nil {
			return time.Time{}, time.Time{}, 0, fmt.Errorf("end_date 格式不正确，应为 YYYY-MM-DD")
		}
		endDate = parsed.Add(24*time.Hour - time.Nanosecond)
	}
	if v, ok := m["limit"].(float64); ok {
		if v < 0 {
			v = 0
		} else if v > float64(maxMcpEntries) {
			v = float64(maxMcpEntries)
		}
		limit = int(v)
	}
	if limit <= 0 {
		limit = defaultMcpPageSize
	}

	if startDate.After(endDate) {
		return time.Time{}, time.Time{}, 0, fmt.Errorf("开始日期不能晚于结束日期")
	}
	if endDate.Sub(startDate) > time.Hour*24*time.Duration(maxMcpDateRangeDays) {
		return time.Time{}, time.Time{}, 0, fmt.Errorf("日期跨度超过限制，最大 %d 天", maxMcpDateRangeDays)
	}
	return startDate, endDate, limit, nil
}

const maxToolTextBytes = maxMcpMessageSize - 512

func appendToolText(b *strings.Builder, s string) bool {
	if b.Len()+len(s) > maxToolTextBytes-len("\n...（内容过长已截断）") {
		b.WriteString("\n...（内容过长已截断）")
		return false
	}
	b.WriteString(s)
	return true
}

func parseStoreMemoryArgs(args json.RawMessage) (createMemoryRequest, error) {
	var req createMemoryRequest
	if len(args) == 0 {
		return req, fmt.Errorf("缺少参数")
	}
	var m map[string]interface{}
	if err := json.Unmarshal(args, &m); err != nil {
		return req, fmt.Errorf("参数格式不正确")
	}

	if v, ok := m["record_time"].(string); ok && v != "" {
		parsed, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return req, fmt.Errorf("record_time 格式不正确，应为 ISO 8601")
		}
		req.RecordTime = parsed
	} else {
		return req, fmt.Errorf("缺少 record_time 参数")
	}

	if v, ok := m["title"].(string); ok {
		req.Title = strings.TrimSpace(v)
	}
	if req.Title == "" || utf8.RuneCountInString(req.Title) > 50 {
		return req, fmt.Errorf("标题最长 50 个字")
	}

	if v, ok := m["content"].(string); ok {
		req.Content = strings.TrimSpace(v)
	}
	if req.Content == "" || utf8.RuneCountInString(req.Content) > 10000 {
		return req, fmt.Errorf("内容需为 1-10000 个字")
	}

	return req, nil
}

func formatMemoryText(items []memoryItem) string {
	if len(items) == 0 {
		return "暂无记录"
	}
	var b strings.Builder
	var currentDate string
	for _, item := range items {
		dateKey := item.CreatedAt.In(timeutil.Shanghai).Format("2006-01-02")
		if dateKey != currentDate {
			if currentDate != "" {
				if !appendToolText(&b, "\n") {
					break
				}
			}
			currentDate = dateKey
			if !appendToolText(&b, "## "+dateKey+"\n") {
				break
			}
		}
		timeStr := item.CreatedAt.In(timeutil.Shanghai).Format("15:04")
		var line strings.Builder
		line.WriteString("- ")
		line.WriteString(timeStr)
		if item.Title != "" {
			line.WriteString(" ")
			line.WriteString(item.Title)
		}
		if item.Location != "" {
			line.WriteString(" ")
			line.WriteString(item.Location)
		}
		line.WriteString("：")
		line.WriteString(strings.TrimSpace(item.Content))
		line.WriteString("\n")
		if !appendToolText(&b, line.String()) {
			break
		}
	}
	return b.String()
}

const memorySyncPrompt = `你是记忆同步助手，负责将本地记忆草稿逐条上报到服务端。

触发：每日凌晨一次（默认00:15–00:45随机），或用户说"上报记忆""同步""归档昨日记忆"时手动执行。

流程：
1. 扫描 {PAPA_MEMORY_TEMP_DIR}/昨天的日期/ 下所有 .md 草稿文件，无则结束。
2. 逐条调用 store_memory（record_time 用草稿日期+时间 ISO 8601，title 不超过 50 字，content 1–10000字）。成功标记 .synced，失败记日志、继续下条。
3. 失败重试最多3次（间隔5分、15分），仅网络超时和5xx重试，4xx跳过。
4. 草稿保留30天，失败条目下次自动补报。

断网兜底：读取本地目录即可浏览，"查看记忆"→近7天列表，"查看6月20日记忆"→当日全文。

配置：PAPA_MEMORY_TEMP_DIR（~/.papa/memory）、PAPA_HUB_URL、PAPA_API_KEY。
注意：只报昨天的记忆，逐条独立不合并，API Key 仅存本地。`

const memoryDigestPrompt = `你是记忆摘要助手，将用户聊天记录或外部转发的AI对话文本生成结构化记忆草稿并存储本地。

触发：用户发外部AI对话文本时立即执行，或每30分钟检查新对话，或用户说"汇总""记录一下""归档"。

流程：
1. 素材收集：AI助手内近一轮聊天 + 外部转发文本（识别Kimi/豆包/千问办公等来源标识的气泡混排）。过滤系统指令、广告等噪音。
2. 清洗角色：按前缀分用户和AI（"我："、"Kimi："等），截图需OCR。
3. 生成记忆：标题 50 字以内概括，正文1–3段保留关键结论。多行输出：日期行、标题行、内容行。
4. 存为草稿：{PAPA_MEMORY_TEMP_DIR}/{日期}/{时间戳}_draft.md。自动触发静默，手动返回"已记录：{标题}"。
5. 支持"查看记忆""查看今天的记录"浏览本地草稿。

配置：PAPA_MEMORY_TEMP_DIR（~/.papa/memory）。
注意：本地处理不上传原始对话，同一天可多条，空素材不生成，LLM失败用清洗后原文兜底。`
