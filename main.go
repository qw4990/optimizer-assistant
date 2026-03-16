package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkcore "github.com/larksuite/oapi-sdk-go/v3/core"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
	larkws "github.com/larksuite/oapi-sdk-go/v3/ws"
)

const defaultSkillURL = "https://github.com/pingcap/agent-rules/blob/main/skills/tidb-query-tuning/SKILL.md"

func main() {
	appID := mustGetEnv("FEISHU_APP_ID")
	appSecret := mustGetEnv("FEISHU_APP_SECRET")

	apiClient := lark.NewClient(appID, appSecret, lark.WithLogLevel(larkcore.LogLevelDebug))
	agent, err := NewQueryAgent(context.Background(), getEnvOrDefault("AGENT_SKILL_URL", defaultSkillURL))
	if err != nil {
		log.Fatalf("init query agent failed: %v", err)
	}

	// Register event handlers. OnP2MessageReceiveV1 handles message receive event v2.0.
	eventHandler := dispatcher.NewEventDispatcher("", "").
		OnP2MessageReceiveV1(func(ctx context.Context, event *larkim.P2MessageReceiveV1) error {
			fmt.Printf("[ OnP2MessageReceiveV1 access ], data: %s\n", larkcore.Prettify(event))

			messageID := extractMessageID(event)
			if messageID == "" {
				log.Printf("skip reply because message_id is empty")
				return nil
			}

			question := extractQuestion(event)
			if question == "" {
				question = "Please introduce your capabilities."
			}

			content, err := agent.Answer(ctx, question)
			if err != nil {
				log.Printf("agent answer failed, message_id=%s, err=%v", messageID, err)
				content, err = buildFeishuPostContentFromText(err.Error(), "Agent Error")
				if err != nil {
					return fmt.Errorf("build error reply content failed: %w", err)
				}
			}

			if err := replyPostMessage(ctx, apiClient, messageID, content); err != nil {
				return err
			}

			return nil
		})
	// Create a websocket client.
	cli := larkws.NewClient(appID, appSecret,
		larkws.WithEventHandler(eventHandler),
		larkws.WithLogLevel(larkcore.LogLevelDebug),
	)
	// Start client and keep websocket connection alive.
	err = cli.Start(context.Background())
	if err != nil {
		panic(err)
	}
}

func getEnvOrDefault(key, defaultValue string) string {
	value := os.Getenv(key)
	if value == "" {
		return defaultValue
	}
	return value
}

func mustGetEnv(key string) string {
	value := os.Getenv(key)
	if value == "" {
		log.Fatalf("%s is required", key)
	}
	return value
}

func extractMessageID(event *larkim.P2MessageReceiveV1) string {
	if event == nil || event.Event == nil || event.Event.Message == nil || event.Event.Message.MessageId == nil {
		return ""
	}
	return *event.Event.Message.MessageId
}

func extractQuestion(event *larkim.P2MessageReceiveV1) string {
	if event == nil || event.Event == nil || event.Event.Message == nil || event.Event.Message.Content == nil {
		return ""
	}

	raw := strings.TrimSpace(*event.Event.Message.Content)
	if raw == "" {
		return ""
	}

	if event.Event.Message.MessageType != nil && *event.Event.Message.MessageType == "text" {
		var payload struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(raw), &payload); err == nil {
			return strings.TrimSpace(payload.Text)
		}
	}
	return raw
}

func replyPostMessage(ctx context.Context, apiClient *lark.Client, messageID, content string) error {
	candidates := []string{content}
	if alternative, ok := togglePostContentWrapper(content); ok && alternative != content {
		candidates = append(candidates, alternative)
	}

	var lastErr error
	for i, candidate := range candidates {
		resp, err := apiClient.Im.V1.Message.Reply(ctx,
			larkim.NewReplyMessageReqBuilder().
				MessageId(messageID).
				Body(larkim.NewReplyMessageReqBodyBuilder().
					MsgType("post").
					Content(candidate).
					Uuid("reply-"+messageID).
					Build()).
				Build())
		if err != nil {
			lastErr = fmt.Errorf("call reply api failed: %w", err)
			continue
		}
		if resp.Success() {
			if i > 0 {
				log.Printf("reply succeeded after switching post content wrapper format, message_id=%s", messageID)
			}
			return nil
		}
		lastErr = fmt.Errorf("reply api failed, code=%d, msg=%s", resp.Code, resp.Msg)
	}
	return lastErr
}

func togglePostContentWrapper(content string) (string, bool) {
	raw := strings.TrimSpace(content)
	if raw == "" {
		return "", false
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return "", false
	}

	if postRaw, ok := obj["post"]; ok {
		return string(postRaw), true
	}

	_, hasZhCN := obj["zh_cn"]
	_, hasEnUS := obj["en_us"]
	_, hasJaJP := obj["ja_jp"]
	if !hasZhCN && !hasEnUS && !hasJaJP {
		return "", false
	}

	wrapped, err := json.Marshal(map[string]json.RawMessage{
		"post": json.RawMessage(raw),
	})
	if err != nil {
		return "", false
	}
	return string(wrapped), true
}
