package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("engbot ")
	cfg, err := LoadConfig()
	if err != nil {
		log.Fatal(err)
	}
	st, err := OpenStore(cfg.DBPath)
	if err != nil {
		log.Fatalf("база данных: %v", err)
	}
	b := &Bot{
		cfg:  cfg,
		tg:   NewTelegram(cfg.TelegramToken),
		llm:  &LLM{g: NewGroq(cfg)},
		st:   st,
		gh:   NewGitHub(cfg),
		expl: map[int64]string{},
		wake: make(chan struct{}, 1),
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("started: model=%s format=%s repo=%s branch=%s proxy=%s prompts=%s",
		cfg.GroqModel, cfg.GroqResponseFormat, cfg.GitHubRepo, cfg.GitHubBranch, ProxyInfo(), PromptVersion)
	b.Run(ctx)
	log.Print("stopped")
}
