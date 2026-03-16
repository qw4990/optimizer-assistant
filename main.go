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

			answer, err := agent.Answer(ctx, question)
			if err != nil {
				log.Printf("agent answer failed, message_id=%s, err=%v", messageID, err)
				answer = "Agent Error: " + err.Error()
			}

			content, err := buildTextContent(answer)
			if err != nil {
				return fmt.Errorf("build text reply content failed: %w", err)
			}

			if err := replyTextMessage(ctx, apiClient, messageID, content); err != nil {
				return fmt.Errorf("reply text message failed: %w", err)
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

func buildTextContent(text string) (string, error) {
	payload := map[string]string{
		"text": strings.TrimSpace(text),
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func replyTextMessage(ctx context.Context, apiClient *lark.Client, messageID, content string) error {
	log.Printf("replying text to message_id=%s, content=%s", messageID, content)
	resp, err := apiClient.Im.V1.Message.Reply(ctx,
		larkim.NewReplyMessageReqBuilder().
			MessageId(messageID).
			Body(larkim.NewReplyMessageReqBodyBuilder().
				MsgType("text").
				Content(content).
				Uuid("reply-"+messageID).
				Build()).
			Build())
	if err != nil {
		return fmt.Errorf("call reply api failed: %w", err)
	}
	if !resp.Success() {
		return fmt.Errorf("reply api failed, code=%d, msg=%s", resp.Code, resp.Msg)
	}
	return nil
}
