package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	defaultKimiBaseURL = "https://api.moonshot.cn"
	defaultKimiModel   = "moonshot-v1-8k"
)

type QueryAgent struct {
	httpClient   *http.Client
	endpoint     string
	apiKey       string
	model        string
	systemPrompt string
}

func NewQueryAgent(ctx context.Context, skillURL string) (*QueryAgent, error) {
	apiKey := strings.TrimSpace(os.Getenv("KIMI_API_KEY"))
	if apiKey == "" {
		return nil, fmt.Errorf("KIMI_API_KEY is required")
	}

	model := getEnvOrDefault("KIMI_MODEL", defaultKimiModel)
	baseURL := getEnvOrDefault("KIMI_BASE_URL", defaultKimiBaseURL)

	skill, err := loadSkillMarkdown(ctx, skillURL)
	if err != nil {
		return nil, err
	}

	return &QueryAgent{
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
		endpoint:     buildKimiEndpoint(baseURL),
		apiKey:       apiKey,
		model:        model,
		systemPrompt: buildSystemPrompt(skill),
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
	reqBody := chatCompletionReq{
		Model: a.model,
		Messages: []chatCompletionMessage{
			{
				Role:    "system",
				Content: a.systemPrompt,
			},
			{
				Role:    "user",
				Content: question,
			},
		},
		Temperature: 0.2,
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal chat completion request failed: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("build chat completion request failed: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+a.apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("call chat completion failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("read chat completion response failed: %w", err)
	}

	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("chat completion failed, status=%d, body=%s", resp.StatusCode, string(respBody))
	}

	var completionResp chatCompletionResp
	if err := json.Unmarshal(respBody, &completionResp); err != nil {
		return "", fmt.Errorf("unmarshal chat completion response failed: %w, body=%s", err, string(respBody))
	}

	if completionResp.Error != nil {
		return "", fmt.Errorf("chat completion API error: %s", completionResp.Error.Message)
	}
	if len(completionResp.Choices) == 0 {
		return "", fmt.Errorf("empty choices from chat completion")
	}

	answer := parseCompletionContent(completionResp.Choices[0].Message.Content)
	if answer == "" {
		return "", fmt.Errorf("empty assistant content from chat completion")
	}
	postContent, err := normalizeFeishuPostContent(answer)
	if err != nil {
		return "", fmt.Errorf("invalid Feishu post content from agent: %w", err)
	}
	return postContent, nil
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
4. Return the final answer as Feishu rich text content JSON for msg_type="post", not Markdown.
5. The output must be valid JSON only (no prose outside JSON, no code fences).
6. Return JSON in one of the following compatible content schemas:
   {
     "zh_cn": {
       "title": "short title",
       "content": [
         [{"tag":"text","text":"line 1"}],
         [{"tag":"text","text":"line 2"}]
       ]
     }
   }
   Or:
   {
     "post": { ...same locale structure... }
   }
   Or:
   {
     "msg_type": "post",
     "content": { ...same locale structure or wrapped post object... }
   }
7. Always provide both "zh_cn" and "en_us" locales with equivalent content.
8. Default to concise, structured, and practical content in English.

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

func normalizeFeishuPostContent(raw string) (string, error) {
	cleaned := stripCodeFence(raw)
	if cleaned == "" {
		return buildFeishuPostContentFromText("No content returned by agent.", "TiDB Query Tuning")
	}

	// Support full message envelope format:
	// {"msg_type":"post","content":{...}}
	// {"msg_type":"post","content":"{...}"}
	var envelope struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal([]byte(cleaned), &envelope); err == nil && len(envelope.Content) > 0 {
		var contentString string
		if err := json.Unmarshal(envelope.Content, &contentString); err == nil {
			cleaned = strings.TrimSpace(contentString)
		} else {
			cleaned = strings.TrimSpace(string(envelope.Content))
		}
	}

	var postContent feishuPostContent
	if err := json.Unmarshal([]byte(cleaned), &postContent); err == nil && postContent.hasLocale() {
		return marshalFeishuPostContent(postContent)
	}

	var wrapped struct {
		Post feishuPostContent `json:"post"`
	}
	if err := json.Unmarshal([]byte(cleaned), &wrapped); err == nil && wrapped.Post.hasLocale() {
		return marshalFeishuPostContent(wrapped.Post)
	}

	// If model does not follow JSON format strictly, degrade gracefully.
	return buildFeishuPostContentFromText(cleaned, "TiDB Query Tuning")
}

func buildFeishuPostContentFromText(text, title string) (string, error) {
	plain := strings.TrimSpace(stripCodeFence(text))
	if plain == "" {
		plain = "No content."
	}

	lines := strings.Split(plain, "\n")
	content := make([][]feishuPostTag, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		content = append(content, []feishuPostTag{{Tag: "text", Text: line}})
	}
	if len(content) == 0 {
		content = append(content, []feishuPostTag{{Tag: "text", Text: plain}})
	}

	postContent := feishuPostContent{
		ZhCN: &feishuPostLocale{
			Title:   strings.TrimSpace(title),
			Content: content,
		},
		EnUS: &feishuPostLocale{
			Title:   strings.TrimSpace(title),
			Content: content,
		},
	}
	return marshalFeishuPostContent(postContent)
}

func marshalFeishuPostContent(postContent feishuPostContent) (string, error) {
	postContent.applyDefaults()
	b, err := json.Marshal(postContent)
	if err != nil {
		return "", fmt.Errorf("marshal Feishu post content failed: %w", err)
	}
	return string(b), nil
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

type feishuPostContent struct {
	ZhCN *feishuPostLocale `json:"zh_cn,omitempty"`
	EnUS *feishuPostLocale `json:"en_us,omitempty"`
}

type feishuPostLocale struct {
	Title   string            `json:"title"`
	Content [][]feishuPostTag `json:"content"`
}

type feishuPostTag struct {
	Tag      string   `json:"tag"`
	Text     string   `json:"text,omitempty"`
	Href     string   `json:"href,omitempty"`
	UserID   string   `json:"user_id,omitempty"`
	UserName string   `json:"user_name,omitempty"`
	Style    []string `json:"style,omitempty"`
}

func (p feishuPostContent) hasLocale() bool {
	return p.ZhCN != nil || p.EnUS != nil
}

func (p *feishuPostContent) applyDefaults() {
	// Keep both locales to maximize rendering compatibility across Feishu client languages.
	if p.ZhCN == nil && p.EnUS != nil {
		p.ZhCN = cloneLocale(p.EnUS)
	}
	if p.EnUS == nil && p.ZhCN != nil {
		p.EnUS = cloneLocale(p.ZhCN)
	}

	if p.ZhCN != nil {
		applyLocaleDefaults(p.ZhCN, "TiDB Query Tuning")
	}
	if p.EnUS != nil {
		applyLocaleDefaults(p.EnUS, "TiDB Query Tuning")
	}
	if p.ZhCN == nil && p.EnUS == nil {
		p.ZhCN = &feishuPostLocale{
			Title:   "TiDB Query Tuning",
			Content: [][]feishuPostTag{{{Tag: "text", Text: "No content."}}},
		}
	}
}

func cloneLocale(src *feishuPostLocale) *feishuPostLocale {
	if src == nil {
		return nil
	}

	dst := &feishuPostLocale{
		Title:   src.Title,
		Content: make([][]feishuPostTag, 0, len(src.Content)),
	}
	for _, paragraph := range src.Content {
		clonedParagraph := make([]feishuPostTag, len(paragraph))
		copy(clonedParagraph, paragraph)
		dst.Content = append(dst.Content, clonedParagraph)
	}
	return dst
}

func applyLocaleDefaults(locale *feishuPostLocale, defaultTitle string) {
	if strings.TrimSpace(locale.Title) == "" {
		locale.Title = defaultTitle
	}

	if len(locale.Content) == 0 {
		locale.Content = [][]feishuPostTag{{{Tag: "text", Text: "No content."}}}
		return
	}

	normalized := make([][]feishuPostTag, 0, len(locale.Content))
	for _, paragraph := range locale.Content {
		if len(paragraph) == 0 {
			continue
		}
		normalized = append(normalized, paragraph)
	}
	if len(normalized) == 0 {
		normalized = [][]feishuPostTag{{{Tag: "text", Text: "No content."}}}
	}
	locale.Content = normalized
}

type chatCompletionReq struct {
	Model       string                  `json:"model"`
	Messages    []chatCompletionMessage `json:"messages"`
	Temperature float64                 `json:"temperature,omitempty"`
}

type chatCompletionMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatCompletionResp struct {
	Choices []chatCompletionChoice `json:"choices"`
	Error   *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type chatCompletionChoice struct {
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}
