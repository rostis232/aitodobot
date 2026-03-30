package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Client struct {
	httpClient *http.Client
	baseURL    string
}

func NewClient() *Client {
	return &Client{
		httpClient: &http.Client{Timeout: 45 * time.Second},
		baseURL:    "https://api.openai.com",
	}
}

func (c *Client) ValidateKey(ctx context.Context, apiKey string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/models", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == 200 {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	return fmt.Errorf("openai key validation failed: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(b)))
}

type ChatMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	Name       string     `json:"name,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
}

type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
}

type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

func (c *Client) ChatCompletion(ctx context.Context, apiKey, model string, messages []ChatMessage, tools []Tool) (ChatMessage, string, error) {
	reqBody := map[string]any{
		"model":       model,
		"temperature": 0.2,
		"messages":    messages,
	}
	if len(tools) > 0 {
		reqBody["tools"] = tools
		reqBody["tool_choice"] = "auto"
	}

	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(reqBody); err != nil {
		return ChatMessage{}, "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/chat/completions", &buf)
	if err != nil {
		return ChatMessage{}, "", err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return ChatMessage{}, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return ChatMessage{}, "", fmt.Errorf("openai chat error: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	var parsed struct {
		Choices []struct {
			FinishReason string      `json:"finish_reason"`
			Message      ChatMessage `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return ChatMessage{}, "", err
	}
	if len(parsed.Choices) == 0 {
		return ChatMessage{}, "", errors.New("openai chat: empty choices")
	}
	return parsed.Choices[0].Message, parsed.Choices[0].FinishReason, nil
}

type ParsedAction struct {
	Action  string     `json:"action"`
	TaskID  *int64     `json:"task_id,omitempty"`
	Text    *string    `json:"text,omitempty"`
	DueDate *string    `json:"due_date,omitempty"`
	Status  *string    `json:"status,omitempty"`
	Filter  *OAIFilter `json:"filter,omitempty"`
}

type OAIFilter struct {
	Status *string `json:"status,omitempty"`
	From   *string `json:"from,omitempty"`
	To     *string `json:"to,omitempty"`
}

func (c *Client) ParseTextToAction(ctx context.Context, apiKey, model, input string) (ParsedAction, error) {
	system := `Ти парсер намірів для менеджера задач. Поверни ТІЛЬКИ валідний JSON без markdown.
Схема:
{
 "action": "create|edit_text|edit_due|edit_status|delete|list|unknown",
 "task_id": number|null,
 "text": string|null,
 "due_date": "YYYY-MM-DD"|null,
 "status": "todo|doing|done"|null,
 "filter": {"status":"todo|doing|done"|null,"from":"YYYY-MM-DD"|null,"to":"YYYY-MM-DD"|null}|null
}
Правила:
- Якщо користувач не вказав task_id для edit/delete, action="unknown".
- due_date може бути null (прибрати дату).
- Якщо це перегляд — action="list" і заповни filter за змістом.
- Якщо це створення — action="create" і text обов'язковий.`

	type msg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}

	reqBody := map[string]any{
		"model":       model,
		"temperature": 0,
		"messages": []msg{
			{Role: "system", Content: system},
			{Role: "user", Content: input},
		},
	}

	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(reqBody); err != nil {
		return ParsedAction{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/chat/completions", &buf)
	if err != nil {
		return ParsedAction{}, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return ParsedAction{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return ParsedAction{}, fmt.Errorf("openai chat error: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return ParsedAction{}, err
	}
	if len(parsed.Choices) == 0 {
		return ParsedAction{}, errors.New("openai chat: empty choices")
	}

	content := strings.TrimSpace(parsed.Choices[0].Message.Content)
	var out ParsedAction
	if err := json.Unmarshal([]byte(content), &out); err != nil {
		return ParsedAction{}, fmt.Errorf("invalid json from model: %w; content=%q", err, content)
	}
	if out.Action == "" {
		return ParsedAction{}, errors.New("parsed action is empty")
	}
	return out, nil
}

func (c *Client) Transcribe(ctx context.Context, apiKey, model, inputAudioPath string) (string, error) {
	f, err := os.Open(inputAudioPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var body bytes.Buffer
	w := multipart.NewWriter(&body)

	if err := w.WriteField("model", model); err != nil {
		return "", err
	}
	if err := w.WriteField("response_format", "text"); err != nil {
		return "", err
	}

	part, err := w.CreateFormFile("file", filepath.Base(inputAudioPath))
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(part, f); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/audio/transcriptions", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("openai transcribe error: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(b)))
	}

	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
