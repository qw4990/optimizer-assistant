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
	defaultOpenAIBaseURL = "https://api.openai.com"
	defaultOpenAIModel   = "gpt-4.1-mini"
)

type QueryAgent struct {
	httpClient   *http.Client
	endpoint     string
	apiKey       string
	model        string
	systemPrompt string
}

func NewQueryAgent(ctx context.Context, skillURL string) (*QueryAgent, error) {
	apiKey := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	if apiKey == "" {
		return nil, fmt.Errorf("OPENAI_API_KEY is required")
	}

	model := getEnvOrDefault("OPENAI_MODEL", defaultOpenAIModel)
	baseURL := getEnvOrDefault("OPENAI_BASE_URL", defaultOpenAIBaseURL)

	skill, err := loadSkillMarkdown(ctx, skillURL)
	if err != nil {
		return nil, err
	}

	return &QueryAgent{
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
		endpoint:     strings.TrimRight(baseURL, "/") + "/v1/chat/completions",
		apiKey:       apiKey,
		model:        model,
		systemPrompt: buildSystemPrompt(skill),
	}, nil
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
	return answer, nil
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
4. Default to concise, structured, and practical responses in English.

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
