package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	urlpkg "net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	defaultKimiBaseURL = "https://api.moonshot.cn"
	defaultKimiModel   = "moonshot-v1-8k"
	defaultToolMaxStep = 8
	defaultFetchTO     = 20
	maxFetchBodyBytes  = 2 << 20
	maxToolResultChars = 12000
)

type QueryAgent struct {
	httpClient       *http.Client
	endpoint         string
	apiKey           string
	model            string
	systemPrompt     string
	maxToolLoopSteps int
	fetchTimeout     time.Duration
}

func NewQueryAgent(ctx context.Context, skillURL string) (*QueryAgent, error) {
	apiKey := strings.TrimSpace(os.Getenv("KIMI_API_KEY"))
	if apiKey == "" {
		return nil, fmt.Errorf("KIMI_API_KEY is required")
	}

	model := getEnvOrDefault("KIMI_MODEL", defaultKimiModel)
	baseURL := getEnvOrDefault("KIMI_BASE_URL", defaultKimiBaseURL)
	maxToolLoopSteps := getEnvAsInt("AGENT_MAX_TOOL_STEPS", defaultToolMaxStep)
	fetchTimeoutSec := getEnvAsInt("AGENT_FETCH_TIMEOUT_SECONDS", defaultFetchTO)

	skill, err := loadSkillMarkdown(ctx, skillURL)
	if err != nil {
		return nil, err
	}

	return &QueryAgent{
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
		endpoint:         buildKimiEndpoint(baseURL),
		apiKey:           apiKey,
		model:            model,
		systemPrompt:     buildSystemPrompt(skill),
		maxToolLoopSteps: maxToolLoopSteps,
		fetchTimeout:     time.Duration(fetchTimeoutSec) * time.Second,
	}, nil
}

func buildKimiEndpoint(baseURL string) string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if strings.HasSuffix(base, "/v1") {
		return base + "/chat/completions"
	}
	return base + "/v1/chat/completions"
}

func (a *QueryAgent) Answer(ctx context.Context, question string) (string, error) {
	return a.runToolLoop(ctx, question)
}

func loadSkillMarkdown(ctx context.Context, skillURL string) (string, error) {
	url := normalizeSkillURL(skillURL)
	if url == "" {
		return "", fmt.Errorf("skill url is empty")
	}

	readCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(readCtx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("build skill request failed: %w", err)
	}
	req.Header.Set("User-Agent", "optimizer-assistant/1.0")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch skill markdown failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return "", fmt.Errorf("read skill markdown failed: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("fetch skill markdown failed, status=%d, body=%s", resp.StatusCode, string(body))
	}

	content := strings.TrimSpace(string(body))
	if content == "" {
		return "", fmt.Errorf("skill markdown is empty: %s", url)
	}
	return content, nil
}

func normalizeSkillURL(url string) string {
	u := strings.TrimSpace(url)
	if u == "" {
		return ""
	}

	const githubBlobPrefix = "https://github.com/"
	if strings.HasPrefix(u, githubBlobPrefix) && strings.Contains(u, "/blob/") {
		u = strings.Replace(u, githubBlobPrefix, "https://raw.githubusercontent.com/", 1)
		u = strings.Replace(u, "/blob/", "/", 1)
	}
	return u
}

func buildSystemPrompt(skill string) string {
	return fmt.Sprintf(`You are a TiDB Query Tuning Agent. You must strictly follow the constraints and workflow defined in the SKILL.md below when answering questions.

Requirements:
1. Prioritize actionable recommendations (SQL, indexes, statistics, and execution-plan analysis steps).
2. If information is insufficient, explicitly list the missing details and provide the smallest next diagnostic steps.
3. Do not invent TiDB syntax, features, or configuration options that do not exist.
4. You can call tool "fetch_url" whenever you need webpage data before answering.
5. Output the final answer as plain text only. Do not use JSON or rich-text syntax.
6. Provide complete and accurate answers with enough detail to be useful.
7. Default language is English.

The following SKILL.md is mandatory guidance:
<skill_md>
%s
</skill_md>`, skill)
}

func parseCompletionContent(raw json.RawMessage) string {
	var plain string
	if err := json.Unmarshal(raw, &plain); err == nil {
		return strings.TrimSpace(plain)
	}

	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var sb strings.Builder
		for _, p := range parts {
			if p.Type == "text" && strings.TrimSpace(p.Text) != "" {
				if sb.Len() > 0 {
					sb.WriteByte('\n')
				}
				sb.WriteString(strings.TrimSpace(p.Text))
			}
		}
		return strings.TrimSpace(sb.String())
	}

	return ""
}

func normalizePlainTextAnswer(raw string) string {
	s := strings.TrimSpace(stripCodeFence(raw))
	if s == "" {
		return ""
	}
	return s
}

func stripCodeFence(raw string) string {
	s := strings.TrimSpace(raw)
	if !strings.HasPrefix(s, "```") {
		return s
	}

	lines := strings.Split(s, "\n")
	if len(lines) < 2 {
		return strings.Trim(s, "`")
	}

	lines = lines[1:]
	if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "```" {
		lines = lines[:len(lines)-1]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func (a *QueryAgent) runToolLoop(ctx context.Context, question string) (string, error) {
	log.Printf("agent_tool_loop_start max_steps=%d question=%q", a.maxToolLoopSteps, summarizeForLog(question, 300))

	messages := []chatCompletionMessage{
		{
			Role:    "system",
			Content: a.systemPrompt,
		},
		{
			Role:    "user",
			Content: question,
		},
	}

	for step := 0; step < a.maxToolLoopSteps; step++ {
		log.Printf("agent_tool_loop_step step=%d message_count=%d", step+1, len(messages))

		resp, err := a.requestChatCompletion(ctx, messages)
		if err != nil {
			log.Printf("agent_tool_loop_request_error step=%d err=%v", step+1, err)
			return "", err
		}
		if len(resp.Choices) == 0 {
			return "", fmt.Errorf("empty choices from chat completion")
		}

		msg := resp.Choices[0].Message
		if len(msg.ToolCalls) == 0 {
			answer := normalizePlainTextAnswer(parseCompletionContent(msg.Content))
			if answer == "" {
				return "", fmt.Errorf("empty assistant answer")
			}
			log.Printf("agent_tool_loop_finish step=%d answer_len=%d answer=%q", step+1, len(answer), summarizeForLog(answer, 500))
			return answer, nil
		}

		assistantContent := parseCompletionContent(msg.Content)
		log.Printf("agent_tool_calls_detected step=%d count=%d assistant_content=%q", step+1, len(msg.ToolCalls), summarizeForLog(assistantContent, 300))
		messages = append(messages, chatCompletionMessage{
			Role:      "assistant",
			Content:   assistantContent,
			ToolCalls: msg.ToolCalls,
		})

		for _, call := range msg.ToolCalls {
			log.Printf("agent_tool_call_start step=%d tool_call_id=%s tool=%s args=%q", step+1, call.ID, call.Function.Name, summarizeForLog(call.Function.Arguments, 500))

			result, toolErr := a.executeToolCall(ctx, call)
			if toolErr != nil {
				log.Printf("agent_tool_call_error step=%d tool_call_id=%s tool=%s err=%v", step+1, call.ID, call.Function.Name, toolErr)
				result = "TOOL_ERROR: " + toolErr.Error()
			} else {
				log.Printf("agent_tool_call_success step=%d tool_call_id=%s tool=%s result=%q", step+1, call.ID, call.Function.Name, summarizeForLog(result, 500))
			}
			messages = append(messages, chatCompletionMessage{
				Role:       "tool",
				ToolCallID: call.ID,
				Content:    result,
			})
		}
	}
	log.Printf("agent_tool_loop_max_steps_exceeded max_steps=%d", a.maxToolLoopSteps)
	return "", fmt.Errorf("agent exceeded max tool loop steps (%d)", a.maxToolLoopSteps)
}

func (a *QueryAgent) requestChatCompletion(ctx context.Context, messages []chatCompletionMessage) (*chatCompletionResp, error) {
	reqBody := chatCompletionReq{
		Model:       a.model,
		Messages:    messages,
		Temperature: 0.2,
		Tools:       buildToolDefinitions(),
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal chat completion request failed: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("build chat completion request failed: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+a.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call chat completion failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("read chat completion response failed: %w", err)
	}

	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("chat completion failed, status=%d, body=%s", resp.StatusCode, string(respBody))
	}

	var completionResp chatCompletionResp
	if err := json.Unmarshal(respBody, &completionResp); err != nil {
		return nil, fmt.Errorf("unmarshal chat completion response failed: %w, body=%s", err, string(respBody))
	}
	if completionResp.Error != nil {
		return nil, fmt.Errorf("chat completion API error: %s", completionResp.Error.Message)
	}
	return &completionResp, nil
}

func buildToolDefinitions() []chatTool {
	return []chatTool{
		{
			Type: "function",
			Function: chatToolFunction{
				Name:        "fetch_url",
				Description: "Fetch webpage content from a URL and return cleaned plain text.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"url": map[string]string{
							"type":        "string",
							"description": "Target webpage URL, must start with http:// or https://",
						},
					},
					"required": []string{"url"},
				},
			},
		},
	}
}

func (a *QueryAgent) executeToolCall(ctx context.Context, call chatCompletionToolCall) (string, error) {
	if call.Type != "function" {
		return "", fmt.Errorf("unsupported tool call type: %s", call.Type)
	}

	switch call.Function.Name {
	case "fetch_url":
		var args struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal([]byte(call.Function.Arguments), &args); err != nil {
			return "", fmt.Errorf("invalid fetch_url args: %w", err)
		}
		if strings.TrimSpace(args.URL) == "" {
			return "", fmt.Errorf("fetch_url requires non-empty url")
		}
		return a.fetchURLTool(ctx, args.URL)
	default:
		return "", fmt.Errorf("unsupported tool name: %s", call.Function.Name)
	}
}

func (a *QueryAgent) fetchURLTool(ctx context.Context, rawURL string) (string, error) {
	u, err := urlpkg.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", fmt.Errorf("invalid url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("unsupported url scheme: %s", u.Scheme)
	}

	toolCtx, cancel := context.WithTimeout(ctx, a.fetchTimeout)
	defer cancel()

	log.Printf("tool_fetch_url_start url=%s timeout=%s", u.String(), a.fetchTimeout)

	req, err := http.NewRequestWithContext(toolCtx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", fmt.Errorf("build fetch request failed: %w", err)
	}
	req.Header.Set("User-Agent", "optimizer-assistant-tool/1.0")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain,application/json,*/*")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch url failed: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchBodyBytes))
	if err != nil {
		return "", fmt.Errorf("read url response failed: %w", err)
	}

	contentType := resp.Header.Get("Content-Type")
	text := string(bodyBytes)
	if strings.Contains(strings.ToLower(contentType), "html") || strings.Contains(strings.ToLower(text), "<html") {
		text = extractTextFromHTML(text)
	} else {
		text = normalizeWhitespaceLines(text)
	}
	text = truncateByRuneCount(text, maxToolResultChars)

	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("fetch url non-2xx: status=%d, content_type=%s, body=%s", resp.StatusCode, contentType, text)
	}

	log.Printf("tool_fetch_url_success url=%s status=%d content_type=%s body_bytes=%d cleaned_chars=%d", u.String(), resp.StatusCode, contentType, len(bodyBytes), len(text))
	return fmt.Sprintf("URL: %s\nStatus: %d\nContent-Type: %s\nContent:\n%s", u.String(), resp.StatusCode, contentType, text), nil
}

var (
	reScriptTag = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	reStyleTag  = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	reHTMLTag   = regexp.MustCompile(`(?s)<[^>]+>`)
)

func extractTextFromHTML(raw string) string {
	s := reScriptTag.ReplaceAllString(raw, " ")
	s = reStyleTag.ReplaceAllString(s, " ")
	s = reHTMLTag.ReplaceAllString(s, "\n")
	s = html.UnescapeString(s)
	return normalizeWhitespaceLines(s)
}

func normalizeWhitespaceLines(raw string) string {
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		out = append(out, strings.Join(fields, " "))
	}
	return strings.Join(out, "\n")
}

func truncateByRuneCount(text string, max int) string {
	if max <= 0 {
		return ""
	}
	rs := []rune(text)
	if len(rs) <= max {
		return text
	}
	return string(rs[:max]) + "\n...(truncated)"
}

func getEnvAsInt(key string, defaultVal int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return defaultVal
	}
	return n
}

func summarizeForLog(raw string, maxRunes int) string {
	normalized := strings.ReplaceAll(raw, "\n", "\\n")
	normalized = strings.TrimSpace(normalized)
	if maxRunes <= 0 {
		return ""
	}
	rs := []rune(normalized)
	if len(rs) <= maxRunes {
		return normalized
	}
	return string(rs[:maxRunes]) + "...(truncated)"
}

type chatCompletionReq struct {
	Model       string                  `json:"model"`
	Messages    []chatCompletionMessage `json:"messages"`
	Temperature float64                 `json:"temperature,omitempty"`
	Tools       []chatTool              `json:"tools,omitempty"`
}

type chatCompletionMessage struct {
	Role       string                   `json:"role"`
	Content    any                      `json:"content,omitempty"`
	ToolCallID string                   `json:"tool_call_id,omitempty"`
	ToolCalls  []chatCompletionToolCall `json:"tool_calls,omitempty"`
}

type chatCompletionResp struct {
	Choices []chatCompletionChoice `json:"choices"`
	Error   *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type chatCompletionChoice struct {
	Message chatCompletionResponseMessage `json:"message"`
}

type chatCompletionResponseMessage struct {
	Content   json.RawMessage          `json:"content"`
	ToolCalls []chatCompletionToolCall `json:"tool_calls,omitempty"`
}

type chatCompletionToolCall struct {
	ID       string                         `json:"id"`
	Type     string                         `json:"type"`
	Function chatCompletionToolCallFunction `json:"function"`
}

type chatCompletionToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatTool struct {
	Type     string           `json:"type"`
	Function chatToolFunction `json:"function"`
}

type chatToolFunction struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}
