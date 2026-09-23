package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"papafeiji/backend/internal/config"
	"papafeiji/backend/internal/db/sqlc"
	"papafeiji/backend/pkg/timeutil"
	"papafeiji/backend/pkg/util"
)

const sseDataPrefix = "data: "

type UpstreamClient struct {
	httpClient *http.Client
	apiKey     string
}

func NewUpstreamClient(cfg *config.Config) *UpstreamClient {
	return &UpstreamClient{
		httpClient: config.SSEHTTPClient(),
		apiKey:     cfg.AIAPIKey,
	}
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type streamRequest struct {
	Model         string          `json:"model"`
	Messages      []chatMessage   `json:"messages"`
	Stream        bool            `json:"stream"`
	MaxTokens     int             `json:"max_tokens,omitempty"`
	Thinking      *thinkingConfig `json:"thinking,omitempty"`
	StreamOptions *streamOptions  `json:"stream_options,omitempty"`
}

type thinkingConfig struct {
	Type string `json:"type"`
}

// streamOptions 请求上游在流末尾返回 token 用量（OpenAI 兼容约定），
// 用于成本观测（msg="ai chat usage" 结构化日志），不落库。
type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type Stream struct {
	reader           io.ReadCloser
	scan             *bufio.Scanner
	parseFailures    int
	promptTokens     int
	completionTokens int
}

func (s *Stream) Next() (string, error) {
	for s.scan.Scan() {
		line := s.scan.Text()
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, sseDataPrefix) {
			continue
		}
		data := strings.TrimPrefix(line, sseDataPrefix)
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			// R2-L02：以 io.EOF 显式标记流结束，与空 chunk（心跳）区分开。
			return "", io.EOF
		}
		var payload struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			s.parseFailures++
			if s.parseFailures > 3 {
				return "", fmt.Errorf("upstream stream parse failed after %d attempts: %w", s.parseFailures, err)
			}
			continue
		}
		s.parseFailures = 0
		// include_usage：流末尾的用量 chunk（choices 为空）只记录，不产出内容。
		if payload.Usage != nil {
			s.promptTokens = payload.Usage.PromptTokens
			s.completionTokens = payload.Usage.CompletionTokens
		}
		if payload.Error != nil && payload.Error.Message != "" {
			return "", fmt.Errorf("upstream error: %s", payload.Error.Message)
		}
		if len(payload.Choices) == 0 {
			continue
		}
		delta := payload.Choices[0].Delta
		if delta.Content != "" {
			return delta.Content, nil
		}

		continue
	}
	if err := s.scan.Err(); err != nil {
		return "", err
	}
	return "", io.EOF
}

func (s *Stream) Close() error {
	if s.reader != nil {
		return s.reader.Close()
	}
	return nil
}

// Usage 返回上游回报的 token 用量（未开启 include_usage 或尚未收到用量 chunk 时为 0）。
func (s *Stream) Usage() (promptTokens, completionTokens int) {
	return s.promptTokens, s.completionTokens
}

func (c *UpstreamClient) Stream(ctx context.Context, baseURL, model, thinkingType string, maxTokens int, messages []chatMessage) (*Stream, error) {
	reqBody := streamRequest{
		Model:         model,
		Messages:      messages,
		Stream:        true,
		MaxTokens:     maxTokens,
		StreamOptions: &streamOptions{IncludeUsage: true},
	}
	if thinkingType != "" {
		reqBody.Thinking = &thinkingConfig{Type: thinkingType}
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	url := strings.TrimSuffix(baseURL, "/") + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close() //nolint:errcheck //nolint:errcheck
		return nil, fmt.Errorf("upstream status %d", resp.StatusCode)
	}

	scan := bufio.NewScanner(resp.Body)
	scan.Buffer(make([]byte, 64*1024), 2*1024*1024)
	return &Stream{reader: resp.Body, scan: scan}, nil
}

// buildMessages assembles chat messages ordered for DeepSeek prefix caching.
//
// Cache strategy:
//
//	messages[0] = system prompt + diary background  ← static prefix, cache hit
//	messages[1] = user identity (optional)          ← dynamic, cache miss
//	messages[2] = conversation history (optional)   ← dynamic, cache miss
//	messages[3] = current user message              ← dynamic, cache miss
//
// messages[0] is identical across requests within the background cache TTL (30 min)
// and same calendar day (nowDate). Do NOT insert dynamic content into messages[0]
// or reorder the array — any change to the message prefix invalidates the cache.
func buildMessages(systemPrompt, nickname, background string, dialogLogs []sqlc.AiDialogLog, userMessage, lang string) []chatMessage {
	var messages []chatMessage

	var sysContent string
	switch lang {
	case "en":
		sysContent = "【强制指令】你必须用英语回答用户的所有问题。无论用户使用什么语言提问，你的回复都只能用英语，禁止使用中文。\n\n" + systemPrompt
	case "zh-Hant":
		sysContent = "請用繁體中文回覆用戶的問題。\n\n" + systemPrompt
	default:
		sysContent = systemPrompt
	}
	if sysContent == "" {
		sysContent = "你是一个日记助手。"
	}

	nowDate := timeutil.NowShanghai().Format("2006年01月02日")
	sysContent = strings.ReplaceAll(sysContent, "{nowDate}", nowDate)
	sysContent = strings.ReplaceAll(sysContent, "{userNickname}", "当前用户")

	if strings.Contains(sysContent, "{userDiaryDetails}") || strings.Contains(sysContent, "{familyDiaryDetails}") {
		diaryContext := background
		if diaryContext == "" {
			diaryContext = "暂无日记记录"
		}
		sysContent = strings.ReplaceAll(sysContent, "{userDiaryDetails}", diaryContext)
		sysContent = strings.ReplaceAll(sysContent, "{familyDiaryDetails}", diaryContext)
	} else if background != "" && background != "暂无日记记录" {
		sysContent += "\n\n以下是我家最近的生活记录，可作为回答背景参考（按时间倒序）：\n" + background
	}

	messages = append(messages, chatMessage{Role: "system", Content: sysContent})

	if nickname != "" {
		identityMsg := "当前提问者是：" + nickname + "。请根据此身份区分\"我\"和家人各自的行程。"
		messages = append(messages, chatMessage{Role: "system", Content: identityMsg})
	}

	if len(dialogLogs) > 0 {
		var contextBuilder strings.Builder
		count := 0
		charCount := 0
		// dialogLogs is ORDER BY created_at DESC (newest first).
		// Iterate newest→oldest so the character limit cuts off stale context, not recent messages.
		for i := 0; i < len(dialogLogs); i++ {
			log := dialogLogs[i]
			content := log.Content
			role := log.Role
			if content == "" {
				continue
			}
			contentLen := utf8.RuneCountInString(content)
			if charCount+contentLen > 2000 {
				break
			}
			if contextBuilder.Len() > 0 {
				contextBuilder.WriteString("\n")
			}
			fmt.Fprintf(&contextBuilder, "%s: %s", role, content)
			charCount += contentLen
			count++
			if count >= 20 {
				break
			}
		}
		if contextBuilder.Len() > 0 {
			messages = append(messages, chatMessage{Role: "system", Content: "近期对话上下文（最近 7 天）：\n" + contextBuilder.String()})
		}
	}

	if lang == "en" {
		userMessage = "(Answer in English) " + userMessage
	}
	messages = append(messages, chatMessage{Role: "user", Content: userMessage})
	return messages
}

func writeSSEData(w http.ResponseWriter, flusher http.Flusher, data string) error {
	lines := strings.Split(data, "\n")
	for _, line := range lines {
		if _, err := fmt.Fprintf(w, "data: %s\n", line); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprint(w, "\n"); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func writeSSEDone(w http.ResponseWriter, flusher http.Flusher) error {
	if _, err := fmt.Fprint(w, "event: done\ndata: \n\n"); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func writeSSEError(w http.ResponseWriter, flusher http.Flusher, code string, bizCode string, message string) error {
	body := map[string]string{"code": code, "message": message}
	if bizCode != "" {
		body["biz_code"] = bizCode
		// 过渡兼容：旧版小程序只识别 camelCase bizCode；待新版本全量发布后移除。
		body["bizCode"] = bizCode
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal sse error: %w", err)
	}
	if _, err := fmt.Fprintf(w, "event: error\ndata: %s\n\n", payload); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func newID() (string, error) {
	return util.NewUUID()
}
