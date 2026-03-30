package bot

import (
	"context"
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

	b.handleNaturalLanguage(ctx, chatID, userID, text)
}

func (b *Bot) handleCommand(ctx context.Context, m *tgbotapi.Message) {
	chatID := m.Chat.ID
	userID := m.From.ID

	switch m.Command() {
	case "start":
		b.sendText(chatID, strings.TrimSpace(`
Я бот для особистих задач.

Команди:
- /add текст ; due=YYYY-MM-DD (опційно)
- /edit_text ID новий текст
- /edit_due ID YYYY-MM-DD | none
- /status ID todo|doing|done
- /delete ID (з підтвердженням)
- /list (опційно: status=..., from=YYYY-MM-DD, to=YYYY-MM-DD)

Також можна писати звичайним текстом або надсилати голосові — використаю OpenAI для розбору (попрошу ваш API ключ один раз).`))
	case "add":
		text, due := parseAddArgs(strings.TrimSpace(m.CommandArguments()))
		if text == "" {
			b.sendText(chatID, "Формат: /add текст ; due=YYYY-MM-DD (опційно)")
			return
		}
		id, err := b.repo.CreateTask(ctx, userID, text, due)
		if err != nil {
			slog.Error("create task", "err", err, "user_id", userID)
			b.sendText(chatID, "Не вдалося створити задачу.")
			return
		}
		slog.Info("task created", "user_id", userID, "task_id", id)
		b.sendText(chatID, fmt.Sprintf("Створено задачу #%d.", id))
	case "edit_text":
		id, rest, ok := parseIDAndRest(m.CommandArguments())
		if !ok || strings.TrimSpace(rest) == "" {
			b.sendText(chatID, "Формат: /edit_text ID новий текст")
			return
		}
		if err := b.repo.UpdateTaskText(ctx, userID, id, strings.TrimSpace(rest)); err != nil {
			slog.Warn("update task text", "err", err, "user_id", userID, "task_id", id)
			b.sendText(chatID, "Не вдалося оновити задачу (перевірте ID).")
			return
		}
		slog.Info("task text updated", "user_id", userID, "task_id", id)
		b.sendText(chatID, "Оновлено текст задачі.")
	case "edit_due":
		id, rest, ok := parseIDAndRest(m.CommandArguments())
		if !ok {
			b.sendText(chatID, "Формат: /edit_due ID YYYY-MM-DD | none")
			return
		}
		var due *time.Time
		arg := strings.TrimSpace(rest)
		if arg != "" && strings.ToLower(arg) != "none" {
			t, err := parseDate(arg)
			if err != nil {
				b.sendText(chatID, "Невірна дата. Формат: YYYY-MM-DD або none")
				return
			}
			due = &t
		}
		if err := b.repo.UpdateTaskDue(ctx, userID, id, due); err != nil {
			slog.Warn("update task due", "err", err, "user_id", userID, "task_id", id)
			b.sendText(chatID, "Не вдалося оновити дату (перевірте ID).")
			return
		}
		slog.Info("task due updated", "user_id", userID, "task_id", id)
		b.sendText(chatID, "Оновлено дату задачі.")
	case "status":
		id, rest, ok := parseIDAndRest(m.CommandArguments())
		if !ok {
			b.sendText(chatID, "Формат: /status ID todo|doing|done")
			return
		}
		st := strings.ToLower(strings.TrimSpace(rest))
		if st != "todo" && st != "doing" && st != "done" {
			b.sendText(chatID, "Статус має бути: todo|doing|done")
			return
		}
		if err := b.repo.UpdateTaskStatus(ctx, userID, id, st); err != nil {
			slog.Warn("update task status", "err", err, "user_id", userID, "task_id", id)
			b.sendText(chatID, "Не вдалося оновити статус (перевірте ID).")
			return
		}
		slog.Info("task status updated", "user_id", userID, "task_id", id, "status", st)
		b.sendText(chatID, "Оновлено статус задачі.")
	case "delete":
		id, _, ok := parseIDAndRest(m.CommandArguments())
		if !ok {
			b.sendText(chatID, "Формат: /delete ID")
			return
		}
		b.askDeleteConfirm(chatID, id)
	case "list":
		f, err := parseListArgs(strings.TrimSpace(m.CommandArguments()))
		if err != nil {
			b.sendText(chatID, "Формат: /list status=todo|doing|done from=YYYY-MM-DD to=YYYY-MM-DD (усе опційно)")
			return
		}
		tasks, err := b.repo.ListTasks(ctx, userID, f)
		if err != nil {
			slog.Error("list tasks", "err", err, "user_id", userID)
			b.sendText(chatID, "Не вдалося отримати список задач.")
			return
		}
		slog.Info("tasks listed", "user_id", userID, "count", len(tasks))
		b.sendText(chatID, formatTasks(tasks))
	default:
		b.sendText(chatID, "Невідома команда. /start для допомоги.")
	}
}

func (b *Bot) handleNaturalLanguage(ctx context.Context, chatID int64, userID int64, input string) {
	apiKey, ok := b.ensureKeyOrAsk(ctx, chatID, userID)
	if !ok {
		return
	}

	act, err := b.openai.ParseTextToAction(ctx, apiKey, b.defaultChatModel, input)
	if err != nil {
		slog.Error("parse action", "err", err, "user_id", userID)
		b.sendText(chatID, "Не вдалося розібрати запит. Спробуйте інакше або використайте команди (/start).")
		return
	}

	switch act.Action {
	case "create":
		if act.Text == nil || strings.TrimSpace(*act.Text) == "" {
			b.sendText(chatID, "Не бачу текст задачі. Напишіть, що треба зробити.")
			return
		}
		var due *time.Time
		if act.DueDate != nil && strings.TrimSpace(*act.DueDate) != "" {
			t, err := parseDate(strings.TrimSpace(*act.DueDate))
			if err == nil {
				due = &t
			}
		}
		id, err := b.repo.CreateTask(ctx, userID, strings.TrimSpace(*act.Text), due)
		if err != nil {
			slog.Error("create task (nl)", "err", err, "user_id", userID)
			b.sendText(chatID, "Не вдалося створити задачу.")
			return
		}
		slog.Info("task created (nl)", "user_id", userID, "task_id", id)
		b.sendText(chatID, fmt.Sprintf("Створено задачу #%d.", id))
	case "edit_text":
		if act.TaskID == nil || act.Text == nil {
			b.sendText(chatID, "Для редагування потрібен ID та новий текст.")
			return
		}
		if err := b.repo.UpdateTaskText(ctx, userID, *act.TaskID, strings.TrimSpace(*act.Text)); err != nil {
			slog.Warn("update task text (nl)", "err", err, "user_id", userID, "task_id", *act.TaskID)
			b.sendText(chatID, "Не вдалося оновити задачу (перевірте ID).")
			return
		}
		slog.Info("task text updated (nl)", "user_id", userID, "task_id", *act.TaskID)
		b.sendText(chatID, "Оновлено текст задачі.")
	case "edit_due":
		if act.TaskID == nil {
			b.sendText(chatID, "Для зміни дати потрібен ID.")
			return
		}
		var due *time.Time
		if act.DueDate != nil && strings.TrimSpace(*act.DueDate) != "" {
			t, err := parseDate(strings.TrimSpace(*act.DueDate))
			if err == nil {
				due = &t
			}
		}
		if err := b.repo.UpdateTaskDue(ctx, userID, *act.TaskID, due); err != nil {
			slog.Warn("update task due (nl)", "err", err, "user_id", userID, "task_id", *act.TaskID)
			b.sendText(chatID, "Не вдалося оновити дату (перевірте ID).")
			return
		}
		slog.Info("task due updated (nl)", "user_id", userID, "task_id", *act.TaskID)
		b.sendText(chatID, "Оновлено дату задачі.")
	case "edit_status":
		if act.TaskID == nil || act.Status == nil {
			b.sendText(chatID, "Для зміни статусу потрібен ID та статус.")
			return
		}
		st := strings.ToLower(strings.TrimSpace(*act.Status))
		if st != "todo" && st != "doing" && st != "done" {
			b.sendText(chatID, "Статус має бути: todo|doing|done")
			return
		}
		if err := b.repo.UpdateTaskStatus(ctx, userID, *act.TaskID, st); err != nil {
			slog.Warn("update task status (nl)", "err", err, "user_id", userID, "task_id", *act.TaskID)
			b.sendText(chatID, "Не вдалося оновити статус (перевірте ID).")
			return
		}
		slog.Info("task status updated (nl)", "user_id", userID, "task_id", *act.TaskID, "status", st)
		b.sendText(chatID, "Оновлено статус задачі.")
	case "delete":
		if act.TaskID == nil {
			b.sendText(chatID, "Для видалення потрібен ID.")
			return
		}
		b.askDeleteConfirm(chatID, *act.TaskID)
	case "list":
		f := repo.ListFilter{}
		if act.Filter != nil {
			if act.Filter.Status != nil {
				s := strings.ToLower(strings.TrimSpace(*act.Filter.Status))
				if s == "todo" || s == "doing" || s == "done" {
					f.Status = &s
				}
			}
			if act.Filter.From != nil && strings.TrimSpace(*act.Filter.From) != "" {
				t, err := parseDate(strings.TrimSpace(*act.Filter.From))
				if err == nil {
					f.From = &t
				}
			}
			if act.Filter.To != nil && strings.TrimSpace(*act.Filter.To) != "" {
				t, err := parseDate(strings.TrimSpace(*act.Filter.To))
				if err == nil {
					f.To = &t
				}
			}
		}
		tasks, err := b.repo.ListTasks(ctx, userID, f)
		if err != nil {
			slog.Error("list tasks (nl)", "err", err, "user_id", userID)
			b.sendText(chatID, "Не вдалося отримати список задач.")
			return
		}
		slog.Info("tasks listed (nl)", "user_id", userID, "count", len(tasks))
		b.sendText(chatID, formatTasks(tasks))
	default:
		b.sendText(chatID, "Не зрозумів запит. Можна використати /start для команд.")
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
	b.handleNaturalLanguage(ctx, chatID, userID, text)
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
		if err := b.repo.DeleteTask(ctx, userID, id); err != nil {
			slog.Warn("delete task", "err", err, "user_id", userID, "task_id", id)
			b.sendText(chatID, "Не вдалося видалити задачу (перевірте ID).")
			return
		}
		slog.Info("task deleted", "user_id", userID, "task_id", id)
		b.sendText(chatID, fmt.Sprintf("Задачу #%d видалено.", id))
		return
	}

	if strings.HasPrefix(data, "cancel_del:") {
		b.sendText(chatID, "Видалення скасовано.")
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
