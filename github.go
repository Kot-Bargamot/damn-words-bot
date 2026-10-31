package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var ErrConflict = errors.New("файл в репозитории изменён вне бота")
var ErrNoToken = errors.New("GITHUB_TOKEN не задан — запись в репозиторий невозможна")

type GitHub struct {
	cfg *Config
	c   *http.Client
}

func NewGitHub(cfg *Config) *GitHub {
	return &GitHub{cfg: cfg, c: httpClient(30 * time.Second)}
}

func (g *GitHub) req(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	u := g.cfg.GitHubAPI + "/repos/" + g.cfg.GitHubRepo + "/contents/" + path
	if method == http.MethodGet {
		u += "?ref=" + url.QueryEscape(g.cfg.GitHubBranch)
	}
	r, _ := http.NewRequestWithContext(ctx, method, u, rd)
	r.Header.Set("Accept", "application/vnd.github+json")
	r.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if g.cfg.GitHubToken != "" {
		r.Header.Set("Authorization", "Bearer "+g.cfg.GitHubToken)
	}
	resp, err := g.c.Do(r)
	if err != nil {
		return nil, fmt.Errorf("github: %w", cleanErr(err))
	}
	return resp, nil
}

// RemoteSHA возвращает sha файла или "" если файла нет.
func (g *GitHub) RemoteSHA(ctx context.Context, path string) (string, error) {
	resp, err := g.req(ctx, http.MethodGet, path, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return "", nil
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("github: http %d", resp.StatusCode)
	}
	var r struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", fmt.Errorf("github: bad response")
	}
	return r.SHA, nil
}

// SyncTopic записывает документ, не перезаписывая чужие изменения.
func (g *GitHub) SyncTopic(ctx context.Context, t *Topic) (string, error) {
	if !strings.HasPrefix(t.Path, "grammar/") || !strings.HasSuffix(t.Path, ".md") || strings.Contains(t.Path, "..") {
		return "", fmt.Errorf("недопустимый путь")
	}
	if g.cfg.GitHubToken == "" {
		return "", ErrNoToken
	}
	remote, err := g.RemoteSHA(ctx, t.Path)
	if err != nil {
		return "", err
	}
	if remote != t.Revision { // файл появился или изменился без участия бота
		return "", ErrConflict
	}
	body := map[string]any{
		"message": "grammar: update " + t.ID,
		"content": base64.StdEncoding.EncodeToString([]byte(t.Markdown)),
		"branch":  g.cfg.GitHubBranch,
	}
	if remote != "" {
		body["sha"] = remote
	}
	resp, err := g.req(ctx, http.MethodPut, t.Path, body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 409 || resp.StatusCode == 422 {
		return "", ErrConflict
	}
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		return "", fmt.Errorf("github: http %d", resp.StatusCode)
	}
	var r struct {
		Content struct {
			SHA string `json:"sha"`
		} `json:"content"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&r)
	return r.Content.SHA, nil
}
