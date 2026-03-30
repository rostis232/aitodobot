package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"

	"tg-tasks-bot/internal/openai"
	"tg-tasks-bot/internal/repo"
)

type Dependencies struct {
	TG               *tgbotapi.BotAPI
	Repo             *repo.Repo
	OpenAI           *openai.Client
	DefaultChatModel string
	TranscribeModel  string
}

type Bot struct {
	tg               *tgbotapi.BotAPI
	repo             *repo.Repo
	openai           *openai.Client
	defaultChatModel string
	transcribeModel  string
}

func New(d Dependencies) *Bot {
	return &Bot{
		tg:               d.TG,
		repo:             d.Repo,
		openai:           d.OpenAI,
		defaultChatModel: d.DefaultChatModel,
		transcribeModel:  d.TranscribeModel,
	}
}

func (b *Bot) Run(ctx context.Context) error {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := b.tg.GetUpdatesChan(u)
	slog.Info("bot started", "username", b.tg.Self.UserName)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case upd := <-updates:
			if upd.CallbackQuery != nil {
				b.handleCallback(ctx, upd.CallbackQuery)
				continue
			}
			if upd.Message == nil {
				continue
			}
			b.handleMessage(ctx, upd.Message)
		}
	}
}

func (b *Bot) handleMessage(ctx context.Context, m *tgbotapi.Message) {
	if m.From == nil {
		return
	}
	userID := m.From.ID
	chatID := m.Chat.ID

	if err := b.repo.EnsureUser(ctx, userID); err != nil {
		slog.Error("ensure user", "err", err)
		b.sendText(chatID, "Помилка БД. Спробуйте пізніше.")
		return
	}

	awaiting, err := b.repo.IsAwaitingKey(ctx, userID)
	if err != nil {
		slog.Error("awaiting key read", "err", err)
		b.sendText(chatID, "Помилка БД. Спробуйте пізніше.")
		return
	}
	if awaiting && m.Text != "" && !m.IsCommand() {
		b.handleIncomingKey(ctx, chatID, userID, strings.TrimSpace(m.Text))
		return
	}

	if m.IsCommand() {
		b.handleCommand(ctx, m)
		return
	}

	if m.Voice != nil {
		b.handleVoice(ctx, m)
		return
	}

	text := strings.TrimSpace(firstNonEmpty(m.Text, m.Caption))
	if text == "" {
		b.sendText(chatID, "Надішліть текст/голос/команду.")
		return
	}

	b.handleChatInput(ctx, chatID, userID, text)
}

func (b *Bot) handleCommand(ctx context.Context, m *tgbotapi.Message) {
	chatID := m.Chat.ID
	userID := m.From.ID

	switch m.Command() {
	case "start":
		b.sendText(chatID, strings.TrimSpace(`
Я бот для особистих задач.
Створено: https://beehivelogic.com/

Пиши мені як у звичайному чаті:
- "Додай задачу купити молоко завтра"
- "Покажи задачі на цей тиждень"
- "Зміни статус задачі 3 на done"
- "Видали задачу 5" (я попрошу підтвердити)

Також можна надсилати голосові — я їх транскрибую і зрозумію як текст.

Перед першим використанням я попрошу ваш OpenAI API key і збережу його зашифровано.`))
	default:
		text := strings.TrimSpace(firstNonEmpty(m.Text, m.Caption))
		if text == "" {
			return
		}
		b.handleChatInput(ctx, chatID, userID, text)
	}
}

func (b *Bot) handleChatInput(ctx context.Context, chatID int64, userID int64, input string) {
	apiKey, ok := b.ensureKeyOrAsk(ctx, chatID, userID)
	if !ok {
		return
	}

	if err := b.repo.AppendChatMessage(ctx, userID, "user", input); err != nil {
		slog.Error("append chat message", "err", err, "user_id", userID)
	}

	history, err := b.repo.LoadRecentChatMessages(ctx, userID, 20)
	if err != nil {
		slog.Error("load chat history", "err", err, "user_id", userID)
		history = nil
	}

	sys := `Ти асистент для управління особистими задачами.
Спілкуйся українською, як у звичайному чаті. Якщо бракує даних (наприклад, ID задачі), постав уточнююче питання.
Статуси задач: todo, doing, done.
Дати приймай у форматі YYYY-MM-DD. Якщо користувач каже "без дати" — прибери due_date (null).
Для роботи з базою завжди використовуй tools. Не вигадуй задачі без tool-виклику.
Для видалення спочатку виклич request_delete_task (щоб бот попросив підтвердження). Після підтвердження користувачем — виклич delete_task.`

	msgs := []openai.ChatMessage{{Role: "system", Content: sys}}
	for _, m := range history {
		role := m.Role
		if role != "user" && role != "assistant" {
			continue
		}
		msgs = append(msgs, openai.ChatMessage{Role: role, Content: m.Content})
	}

	tools := b.taskTools()

	var final string
	for i := 0; i < 6; i++ {
		assistant, _, err := b.openai.ChatCompletion(ctx, apiKey, b.defaultChatModel, msgs, tools)
		if err != nil {
			slog.Error("openai chat completion", "err", err, "user_id", userID)
			b.sendText(chatID, "Не вдалося відповісти. Спробуйте ще раз.")
			return
		}

		msgs = append(msgs, assistant)

		if len(assistant.ToolCalls) == 0 {
			final = strings.TrimSpace(assistant.Content)
			break
		}

		for _, tc := range assistant.ToolCalls {
			out := b.executeToolCall(ctx, chatID, userID, tc)
			msgs = append(msgs, openai.ChatMessage{
				Role:       "tool",
				ToolCallID: tc.ID,
				Content:    out,
			})
		}
	}

	if final == "" {
		final = "Готово."
	}

	if err := b.repo.AppendChatMessage(ctx, userID, "assistant", final); err != nil {
		slog.Error("append chat message", "err", err, "user_id", userID)
	}
	b.sendText(chatID, final)
}

func (b *Bot) taskTools() []openai.Tool {
	return []openai.Tool{
		{
			Type: "function",
			Function: openai.ToolFunction{
				Name:        "create_task",
				Description: "Створити нову задачу",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"text": map[string]any{
							"type":        "string",
							"description": "Текст задачі",
						},
						"due_date": map[string]any{
							"type":        []any{"string", "null"},
							"description": "Дата виконання YYYY-MM-DD або null якщо без дати",
						},
					},
					"required": []any{"text"},
				},
			},
		},
		{
			Type: "function",
			Function: openai.ToolFunction{
				Name:        "update_task_text",
				Description: "Змінити текст задачі",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"task_id": map[string]any{"type": "integer"},
						"text":    map[string]any{"type": "string"},
					},
					"required": []any{"task_id", "text"},
				},
			},
		},
		{
			Type: "function",
			Function: openai.ToolFunction{
				Name:        "update_task_due",
				Description: "Змінити або прибрати дату виконання задачі",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"task_id": map[string]any{"type": "integer"},
						"due_date": map[string]any{
							"type":        []any{"string", "null"},
							"description": "YYYY-MM-DD або null щоб прибрати дату",
						},
					},
					"required": []any{"task_id", "due_date"},
				},
			},
		},
		{
			Type: "function",
			Function: openai.ToolFunction{
				Name:        "update_task_status",
				Description: "Змінити статус задачі",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"task_id": map[string]any{"type": "integer"},
						"status":  map[string]any{"type": "string", "enum": []any{"todo", "doing", "done"}},
					},
					"required": []any{"task_id", "status"},
				},
			},
		},
		{
			Type: "function",
			Function: openai.ToolFunction{
				Name:        "list_tasks",
				Description: "Показати список задач з фільтрами",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"status": map[string]any{"type": []any{"string", "null"}, "description": "todo|doing|done або null"},
						"from":   map[string]any{"type": []any{"string", "null"}, "description": "YYYY-MM-DD або null"},
						"to":     map[string]any{"type": []any{"string", "null"}, "description": "YYYY-MM-DD або null"},
					},
				},
			},
		},
		{
			Type: "function",
			Function: openai.ToolFunction{
				Name:        "request_delete_task",
				Description: "Запросити підтвердження видалення задачі через кнопки в Telegram",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"task_id": map[string]any{"type": "integer"},
					},
					"required": []any{"task_id"},
				},
			},
		},
		{
			Type: "function",
			Function: openai.ToolFunction{
				Name:        "delete_task",
				Description: "Видалити задачу (використовувати тільки після підтвердження користувачем)",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"task_id": map[string]any{"type": "integer"},
					},
					"required": []any{"task_id"},
				},
			},
		},
		{
			Type: "function",
			Function: openai.ToolFunction{
				Name:        "cancel_delete",
				Description: "Скасувати pending видалення (якщо було)",
				Parameters: map[string]any{
					"type":       "object",
					"properties": map[string]any{},
				},
			},
		},
	}
}

func (b *Bot) executeToolCall(ctx context.Context, chatID int64, userID int64, tc openai.ToolCall) string {
	mustJSON := func(v any) string {
		b, _ := json.Marshal(v)
		return string(b)
	}

	switch tc.Function.Name {
	case "create_task":
		var args struct {
			Text    string  `json:"text"`
			DueDate *string `json:"due_date"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return mustJSON(map[string]any{"ok": false, "error": "bad arguments"})
		}
		text := strings.TrimSpace(args.Text)
		if text == "" {
			return mustJSON(map[string]any{"ok": false, "error": "text is required"})
		}
		var due *time.Time
		if args.DueDate != nil && strings.TrimSpace(*args.DueDate) != "" {
			t, err := parseDate(strings.TrimSpace(*args.DueDate))
			if err != nil {
				return mustJSON(map[string]any{"ok": false, "error": "invalid due_date"})
			}
			due = &t
		}
		id, err := b.repo.CreateTask(ctx, userID, text, due)
		if err != nil {
			slog.Error("tool create_task", "err", err, "user_id", userID)
			return mustJSON(map[string]any{"ok": false, "error": "db error"})
		}
		return mustJSON(map[string]any{"ok": true, "task_id": id})

	case "update_task_text":
		var args struct {
			TaskID int64  `json:"task_id"`
			Text   string `json:"text"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return mustJSON(map[string]any{"ok": false, "error": "bad arguments"})
		}
		text := strings.TrimSpace(args.Text)
		if text == "" {
			return mustJSON(map[string]any{"ok": false, "error": "text is required"})
		}
		if err := b.repo.UpdateTaskText(ctx, userID, args.TaskID, text); err != nil {
			return mustJSON(map[string]any{"ok": false, "error": err.Error()})
		}
		return mustJSON(map[string]any{"ok": true})

	case "update_task_due":
		var args struct {
			TaskID  int64   `json:"task_id"`
			DueDate *string `json:"due_date"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return mustJSON(map[string]any{"ok": false, "error": "bad arguments"})
		}
		var due *time.Time
		if args.DueDate != nil && strings.TrimSpace(*args.DueDate) != "" {
			t, err := parseDate(strings.TrimSpace(*args.DueDate))
			if err != nil {
				return mustJSON(map[string]any{"ok": false, "error": "invalid due_date"})
			}
			due = &t
		}
		if err := b.repo.UpdateTaskDue(ctx, userID, args.TaskID, due); err != nil {
			return mustJSON(map[string]any{"ok": false, "error": err.Error()})
		}
		return mustJSON(map[string]any{"ok": true})

	case "update_task_status":
		var args struct {
			TaskID int64  `json:"task_id"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return mustJSON(map[string]any{"ok": false, "error": "bad arguments"})
		}
		st := strings.ToLower(strings.TrimSpace(args.Status))
		if st != "todo" && st != "doing" && st != "done" {
			return mustJSON(map[string]any{"ok": false, "error": "invalid status"})
		}
		if err := b.repo.UpdateTaskStatus(ctx, userID, args.TaskID, st); err != nil {
			return mustJSON(map[string]any{"ok": false, "error": err.Error()})
		}
		return mustJSON(map[string]any{"ok": true})

	case "list_tasks":
		var args struct {
			Status *string `json:"status"`
			From   *string `json:"from"`
			To     *string `json:"to"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return mustJSON(map[string]any{"ok": false, "error": "bad arguments"})
		}
		f := repo.ListFilter{}
		if args.Status != nil && strings.TrimSpace(*args.Status) != "" {
			s := strings.ToLower(strings.TrimSpace(*args.Status))
			if s == "todo" || s == "doing" || s == "done" {
				f.Status = &s
			}
		}
		if args.From != nil && strings.TrimSpace(*args.From) != "" {
			t, err := parseDate(strings.TrimSpace(*args.From))
			if err == nil {
				f.From = &t
			}
		}
		if args.To != nil && strings.TrimSpace(*args.To) != "" {
			t, err := parseDate(strings.TrimSpace(*args.To))
			if err == nil {
				f.To = &t
			}
		}
		tasks, err := b.repo.ListTasks(ctx, userID, f)
		if err != nil {
			return mustJSON(map[string]any{"ok": false, "error": "db error"})
		}
		type outTask struct {
			ID      int64   `json:"id"`
			Text    string  `json:"text"`
			DueDate *string `json:"due_date"`
			Status  string  `json:"status"`
		}
		out := make([]outTask, 0, len(tasks))
		for _, t := range tasks {
			var due *string
			if t.DueDate != nil {
				s := t.DueDate.Format("2006-01-02")
				due = &s
			}
			out = append(out, outTask{ID: t.ID, Text: t.Text, DueDate: due, Status: t.Status})
		}
		return mustJSON(map[string]any{"ok": true, "tasks": out})

	case "request_delete_task":
		var args struct {
			TaskID int64 `json:"task_id"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return mustJSON(map[string]any{"ok": false, "error": "bad arguments"})
		}
		if err := b.repo.SetPendingDelete(ctx, userID, args.TaskID); err != nil {
			return mustJSON(map[string]any{"ok": false, "error": "db error"})
		}
		b.askDeleteConfirm(chatID, args.TaskID)
		return mustJSON(map[string]any{"ok": true, "confirmation": "requested", "task_id": args.TaskID})

	case "delete_task":
		var args struct {
			TaskID int64 `json:"task_id"`
		}
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return mustJSON(map[string]any{"ok": false, "error": "bad arguments"})
		}
		pendingID, ok, err := b.repo.GetPendingDelete(ctx, userID)
		if err != nil {
			return mustJSON(map[string]any{"ok": false, "error": "db error"})
		}
		if !ok || pendingID != args.TaskID {
			return mustJSON(map[string]any{"ok": false, "error": "not confirmed"})
		}
		if err := b.repo.DeleteTask(ctx, userID, args.TaskID); err != nil {
			return mustJSON(map[string]any{"ok": false, "error": err.Error()})
		}
		_ = b.repo.ClearPendingDelete(ctx, userID)
		return mustJSON(map[string]any{"ok": true})

	case "cancel_delete":
		_ = b.repo.ClearPendingDelete(ctx, userID)
		return mustJSON(map[string]any{"ok": true})
	default:
		return mustJSON(map[string]any{"ok": false, "error": "unknown tool"})
	}
}

func (b *Bot) handleVoice(ctx context.Context, m *tgbotapi.Message) {
	chatID := m.Chat.ID
	userID := m.From.ID

	apiKey, ok := b.ensureKeyOrAsk(ctx, chatID, userID)
	if !ok {
		return
	}

	url, err := b.tg.GetFileDirectURL(m.Voice.FileID)
	if err != nil {
		slog.Error("tg file url", "err", err, "user_id", userID)
		b.sendText(chatID, "Не вдалося отримати голосове повідомлення.")
		return
	}

	tmpDir := os.TempDir()
	inPath := filepath.Join(tmpDir, fmt.Sprintf("voice_%d_%d.oga", userID, time.Now().UnixNano()))
	outPath := filepath.Join(tmpDir, fmt.Sprintf("voice_%d_%d.wav", userID, time.Now().UnixNano()))

	defer func() {
		_ = os.Remove(inPath)
		_ = os.Remove(outPath)
	}()

	if err := downloadToFile(url, inPath); err != nil {
		slog.Error("download voice", "err", err, "user_id", userID)
		b.sendText(chatID, "Не вдалося завантажити голосове повідомлення.")
		return
	}

	if err := convertToWav(inPath, outPath); err != nil {
		slog.Error("ffmpeg convert", "err", err, "user_id", userID)
		b.sendText(chatID, "Не вдалося обробити аудіо (ffmpeg).")
		return
	}

	text, err := b.openai.Transcribe(ctx, apiKey, b.transcribeModel, outPath)
	if err != nil {
		slog.Error("transcribe", "err", err, "user_id", userID)
		b.sendText(chatID, "Не вдалося транскрибувати аудіо.")
		return
	}
	if strings.TrimSpace(text) == "" {
		b.sendText(chatID, "Не вдалося розпізнати текст з аудіо.")
		return
	}

	slog.Info("voice transcribed", "user_id", userID)
	b.handleChatInput(ctx, chatID, userID, text)
}

func (b *Bot) ensureKeyOrAsk(ctx context.Context, chatID int64, userID int64) (string, bool) {
	has, err := b.repo.HasOpenAIKey(ctx, userID)
	if err != nil {
		slog.Error("has key", "err", err, "user_id", userID)
		b.sendText(chatID, "Помилка БД. Спробуйте пізніше.")
		return "", false
	}
	if !has {
		_ = b.repo.SetAwaitingKey(ctx, userID, true)
		b.sendText(chatID, "Надішліть ваш OpenAI API key одним повідомленням (збережу зашифровано).")
		return "", false
	}
	key, err := b.repo.LoadOpenAIKey(ctx, userID)
	if err != nil {
		_ = b.repo.SetAwaitingKey(ctx, userID, true)
		b.sendText(chatID, "Надішліть ваш OpenAI API key одним повідомленням (збережу зашифровано).")
		return "", false
	}
	return key, true
}

func (b *Bot) handleIncomingKey(ctx context.Context, chatID int64, userID int64, candidate string) {
	if candidate == "" {
		b.sendText(chatID, "Ключ порожній. Надішліть OpenAI API key ще раз.")
		return
	}
	if err := b.openai.ValidateKey(ctx, candidate); err != nil {
		slog.Warn("openai key validation failed", "user_id", userID, "err", err)
		b.sendText(chatID, "Ключ не пройшов валідацію. Перевірте та надішліть ще раз.")
		return
	}
	if err := b.repo.StoreOpenAIKey(ctx, userID, candidate); err != nil {
		slog.Error("store key", "err", err, "user_id", userID)
		b.sendText(chatID, "Не вдалося зберегти ключ. Спробуйте пізніше.")
		return
	}
	slog.Info("openai key stored", "user_id", userID)
	b.sendText(chatID, "Ключ збережено. Можна користуватись голосом/вільним текстом.")
}

func (b *Bot) handleCallback(ctx context.Context, q *tgbotapi.CallbackQuery) {
	if q.From == nil || q.Message == nil {
		return
	}
	userID := q.From.ID
	chatID := q.Message.Chat.ID

	_, _ = b.tg.Request(tgbotapi.NewCallback(q.ID, ""))

	data := q.Data
	if strings.HasPrefix(data, "del:") {
		parts := strings.Split(data, ":")
		if len(parts) != 2 {
			return
		}
		id, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			return
		}
		b.handleChatInput(ctx, chatID, userID, fmt.Sprintf("Підтверджую видалення задачі %d", id))
		return
	}

	if strings.HasPrefix(data, "cancel_del:") {
		b.handleChatInput(ctx, chatID, userID, "Скасувати видалення")
		return
	}
}

func (b *Bot) askDeleteConfirm(chatID int64, taskID int64) {
	kb := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Так, видалити", fmt.Sprintf("del:%d", taskID)),
			tgbotapi.NewInlineKeyboardButtonData("Скасувати", fmt.Sprintf("cancel_del:%d", taskID)),
		),
	)
	msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("Підтвердити видалення задачі #%d?", taskID))
	msg.ReplyMarkup = kb
	_, _ = b.tg.Send(msg)
}

func (b *Bot) sendText(chatID int64, text string) {
	_, _ = b.tg.Send(tgbotapi.NewMessage(chatID, text))
}

func downloadToFile(url, dst string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("unexpected status: %d", resp.StatusCode)
	}

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	return err
}

func convertToWav(inPath, outPath string) error {
	cmd := exec.Command("ffmpeg", "-y", "-i", inPath, "-ac", "1", "-ar", "16000", outPath)
	return cmd.Run()
}

func parseDate(s string) (time.Time, error) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, err
	}
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC), nil
}

func parseAddArgs(s string) (string, *time.Time) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	parts := strings.Split(s, ";")
	text := strings.TrimSpace(parts[0])
	var due *time.Time
	if len(parts) > 1 {
		for _, p := range parts[1:] {
			p = strings.TrimSpace(p)
			if strings.HasPrefix(strings.ToLower(p), "due=") {
				ds := strings.TrimSpace(p[4:])
				if ds == "" {
					continue
				}
				if t, err := parseDate(ds); err == nil {
					due = &t
				}
			}
		}
	}
	return text, due
}

func parseIDAndRest(s string) (int64, string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, "", false
	}
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return 0, "", false
	}
	id, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return 0, "", false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(s, fields[0]))
	return id, rest, true
}

func parseListArgs(s string) (repo.ListFilter, error) {
	var f repo.ListFilter
	s = strings.TrimSpace(s)
	if s == "" {
		return f, nil
	}
	for _, tok := range strings.Fields(s) {
		if strings.HasPrefix(tok, "status=") {
			v := strings.ToLower(strings.TrimPrefix(tok, "status="))
			if v != "todo" && v != "doing" && v != "done" {
				return f, errors.New("bad status")
			}
			f.Status = &v
		}
		if strings.HasPrefix(tok, "from=") {
			v := strings.TrimPrefix(tok, "from=")
			t, err := parseDate(v)
			if err != nil {
				return f, err
			}
			f.From = &t
		}
		if strings.HasPrefix(tok, "to=") {
			v := strings.TrimPrefix(tok, "to=")
			t, err := parseDate(v)
			if err != nil {
				return f, err
			}
			f.To = &t
		}
	}
	return f, nil
}

func formatTasks(tasks []repo.Task) string {
	if len(tasks) == 0 {
		return "Список задач порожній."
	}
	var sb strings.Builder
	for _, t := range tasks {
		due := "-"
		if t.DueDate != nil {
			due = t.DueDate.Format("2006-01-02")
		}
		sb.WriteString(fmt.Sprintf("#%d [%s] due:%s\n%s\n\n", t.ID, t.Status, due, t.Text))
	}
	return strings.TrimSpace(sb.String())
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
