// backupd: UI web + agendador de backups num único processo.
//
// O binário não sabe fazer backup de nada — ele só executa os mesmos scripts
// da imagem (backup.sh, restore.sh, drivers/*.sh), que continuam funcionando
// sozinhos via CLI. Manter os scripts como única fonte da lógica de backup é o
// que deixa a UI opcional e os engines futuros plugáveis.
package main

import (
	"crypto/rand"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type config struct {
	port          string
	adminUser     string
	adminPassword string
	sessionTTL    time.Duration
	cookieSecure  string // "auto" | "true" | "false"
	backupDir     string
	driversDir    string
	scriptsDir    string
	interval      time.Duration
	retry         time.Duration
	runOnStart    bool
	publicHost    string // de PUBLIC_URL; vazio = só r.Host é aceito como Origin
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envFloat(key string, def float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		log.Fatalf("[Config] %s inválido: %q", key, v)
	}
	return f
}

func loadConfig() config {
	pw := os.Getenv("ADMIN_PASSWORD")
	if pw == "" {
		log.Fatal("[Config] ADMIN_PASSWORD não definida. A UI web exige senha; " +
			"defina ADMIN_PASSWORD ou rode o container no modo 'daemon' (sem web).")
	}
	if len(pw) < 8 {
		log.Fatal("[Config] ADMIN_PASSWORD precisa ter pelo menos 8 caracteres.")
	}
	return config{
		port:          envOr("WEB_PORT", "8080"),
		adminUser:     envOr("ADMIN_USER", "admin"),
		adminPassword: pw,
		sessionTTL:    time.Duration(envFloat("SESSION_TTL_HOURS", 12) * float64(time.Hour)),
		cookieSecure:  envOr("COOKIE_SECURE", "auto"),
		backupDir:     envOr("BACKUP_DIR", "/backups"),
		driversDir:    envOr("DRIVERS_DIR", "/usr/local/bin/drivers"),
		scriptsDir:    envOr("SCRIPTS_DIR", "/usr/local/bin"),
		interval:      time.Duration(envFloat("INTERVAL_HOURS", 6) * float64(time.Hour)),
		retry:         time.Duration(envFloat("RETRY_MINUTES", 15) * float64(time.Minute)),
		runOnStart:    strings.EqualFold(envOr("RUN_ON_START", "true"), "true"),
		publicHost:    publicHost(),
	}
}

// PUBLIC_URL é o único jeito são de aceitar um Origin diferente de r.Host: é
// o admin, dono do env do container, quem declara o domínio público — nunca
// um header vindo do próprio cliente HTTP. Necessário quando um reverse proxy
// (Cloudflare, nginx, Traefik...) na frente do container não repassa o Host
// original intacto (ex.: nginx sem `proxy_set_header Host $host;`), o que faz
// r.Host chegar diferente do Origin que o navegador manda.
func publicHost() string {
	raw := os.Getenv("PUBLIC_URL")
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		log.Fatalf("[Config] PUBLIC_URL inválida: %q (esperado algo como https://backup.exemplo.com)", raw)
	}
	return u.Hostname()
}

func sessionSecret() []byte {
	if s := os.Getenv("SESSION_SECRET"); s != "" {
		// segredo curto deixa forjar sessão por força bruta offline do HMAC
		if len(s) < 32 {
			log.Fatal("[Config] SESSION_SECRET precisa ter pelo menos 32 caracteres (ex.: openssl rand -hex 32).")
		}
		return []byte(s)
	}
	// Sem SESSION_SECRET as sessões caem a cada restart do container —
	// aceitável para um único admin.
	return randomSecret()
}

func randomSecret() []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		log.Fatalf("[Config] falha ao gerar segredo de sessão: %v", err)
	}
	return b
}

// Mesmo ciclo do modo daemon do entrypoint: roda, espera o intervalo,
// e encurta a espera quando o ciclo falha.
func (s *server) scheduler() {
	if !s.cfg.runOnStart {
		time.Sleep(s.cfg.interval)
	}
	for {
		job, err := s.jobs.start("backup agendado", s.backupCmd(""))
		if err != nil {
			// um job manual está rodando; tenta de novo mais tarde
			time.Sleep(s.cfg.retry)
			continue
		}
		<-job.done
		if job.ok() {
			time.Sleep(s.cfg.interval)
		} else {
			log.Printf("[Agendador] ciclo falhou; nova tentativa em %s", s.cfg.retry)
			time.Sleep(s.cfg.retry)
		}
	}
}

func main() {
	cfg := loadConfig()
	// A senha entra na chave do HMAC: trocar ADMIN_PASSWORD invalida as
	// sessões abertas mesmo com SESSION_SECRET fixo.
	srv := newServer(cfg, append(sessionSecret(), cfg.adminPassword...), newJobManager())
	go srv.scheduler()
	log.Printf("[Web] UI em :%s (usuário %s); backup a cada %s", cfg.port, cfg.adminUser, cfg.interval)
	// Timeouts: sem eles um cliente lento (Slowloris) segura conexões para sempre.
	hs := &http.Server{
		Addr:              ":" + cfg.port,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	log.Fatal(hs.ListenAndServe())
}
