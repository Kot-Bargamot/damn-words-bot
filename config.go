package main

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config — все настройки берутся из переменных окружения.
// Прокси задаётся на уровне контейнера стандартными HTTPS_PROXY/HTTP_PROXY/NO_PROXY
// (см. docker-entrypoint.sh) и применяется ко всем исходящим запросам.
type Config struct {
	TelegramToken string
	AllowedUserID int64

	GroqKey             string
	GroqModel           string
	GroqBaseURL         string
	GroqTimeout         time.Duration
	GroqRetries         int
	GroqMaxTokens       int
	GroqTemperature     float64
	GroqReasoningEffort string
	GroqResponseFormat  string // strict | best_effort | json_object
	GroqMinInterval     time.Duration

	GitHubRepo   string // owner/name
	GitHubBranch string
	GitHubToken  string // нужен для записи; чтение публичного репо работает без него
	GitHubAPI    string

	DBPath        string
	MaxItems      int
	SessionTasks  int
	NewPerSession int
	DialogueTurns int
	Ladder        []int
}

func LoadConfig() (*Config, error) {
	c := &Config{}
	var missing []string
	req := func(k string) string {
		v := strings.TrimSpace(os.Getenv(k))
		if v == "" {
			missing = append(missing, k)
		}
		return v
	}
	c.TelegramToken = req("TELEGRAM_BOT_TOKEN")
	c.GroqKey = req("GROQ_API_KEY")
	uid := req("ALLOWED_USER_ID")
	c.GitHubRepo = req("GITHUB_REPO")
	if len(missing) > 0 {
		return nil, fmt.Errorf("не заданы переменные окружения: %s", strings.Join(missing, ", "))
	}
	var err error
	if c.AllowedUserID, err = strconv.ParseInt(uid, 10, 64); err != nil {
		return nil, fmt.Errorf("ALLOWED_USER_ID должен быть числовым Telegram user ID")
	}
	if p := strings.Split(c.GitHubRepo, "/"); len(p) != 2 || p[0] == "" || p[1] == "" {
		return nil, fmt.Errorf("GITHUB_REPO должен иметь вид owner/name")
	}

	c.GroqModel = env("GROQ_MODEL", "openai/gpt-oss-120b")
	c.GroqBaseURL = strings.TrimRight(env("GROQ_BASE_URL", "https://api.groq.com/openai/v1"), "/")
	c.GroqTimeout = time.Duration(envInt("GROQ_TIMEOUT_SEC", 60)) * time.Second
	c.GroqRetries = envInt("GROQ_RETRIES", 2)
	c.GroqMaxTokens = envInt("GROQ_MAX_TOKENS", 4096)
	c.GroqMinInterval = time.Duration(envInt("GROQ_MIN_INTERVAL_MS", 1000)) * time.Millisecond
	if c.GroqTemperature, err = strconv.ParseFloat(env("GROQ_TEMPERATURE", "0.5"), 64); err != nil {
		return nil, fmt.Errorf("GROQ_TEMPERATURE должен быть числом")
	}
	c.GroqResponseFormat = env("GROQ_RESPONSE_FORMAT", "strict")
	switch c.GroqResponseFormat {
	case "strict", "best_effort", "json_object":
	default:
		return nil, fmt.Errorf("GROQ_RESPONSE_FORMAT: strict, best_effort или json_object")
	}
	// По умолчанию «низкие» рассуждения для gpt-oss (экономия лимитов); "none" — не передавать параметр.
	c.GroqReasoningEffort = os.Getenv("GROQ_REASONING_EFFORT")
	if c.GroqReasoningEffort == "" && strings.HasPrefix(c.GroqModel, "openai/gpt-oss") {
		c.GroqReasoningEffort = "low"
	}
	if c.GroqReasoningEffort == "none" {
		c.GroqReasoningEffort = ""
	}

	c.GitHubBranch = env("GITHUB_BRANCH", "main")
	c.GitHubToken = os.Getenv("GITHUB_TOKEN")
	c.GitHubAPI = strings.TrimRight(env("GITHUB_API_URL", "https://api.github.com"), "/")

	c.DBPath = env("DB_PATH", "data/engbot.db")
	c.MaxItems = envInt("MAX_ITEMS_PER_MESSAGE", 7)
	c.SessionTasks = envInt("SESSION_TASKS", 5)
	c.NewPerSession = envInt("NEW_ITEMS_PER_SESSION", 2)
	c.DialogueTurns = envInt("DIALOGUE_TURNS", 4)
	for _, s := range strings.Split(env("SRS_LADDER_DAYS", "1,3,7,14,30,60"), ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("SRS_LADDER_DAYS: ожидаются положительные числа через запятую")
		}
		c.Ladder = append(c.Ladder, n)
	}
	return c, nil
}

// ProxyInfo — описание прокси из окружения для логов (без логина и пароля).
func ProxyInfo() string {
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		if v := os.Getenv(k); v != "" {
			if u, err := url.Parse(v); err == nil && u.Host != "" {
				return u.Scheme + "://" + u.Host
			}
			return "invalid"
		}
	}
	return "none"
}

func env(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
