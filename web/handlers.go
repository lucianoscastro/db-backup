package main

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed static
var staticFS embed.FS

type server struct {
	cfg     config
	mu      sync.RWMutex // protege secret, trocado no logout
	secret  []byte
	jobs    *jobManager
	limiter *loginLimiter
	engines []engineInfo
}

type engineInfo struct {
	Name  string `json:"name"`
	Label string `json:"label"`
	Host  string `json:"host"`
}

func newServer(cfg config, secret []byte, jobs *jobManager) *server {
	return &server{
		cfg:     cfg,
		secret:  secret,
		jobs:    jobs,
		limiter: newLoginLimiter(),
		engines: discoverEngines(cfg.driversDir),
	}
}

// Os engines vêm dos próprios drivers (contrato enabled/info): um driver novo
// aparece na UI sem mudar o Go. Config é env, então descobrir uma vez basta.
func discoverEngines(dir string) []engineInfo {
	var out []engineInfo
	paths, _ := filepath.Glob(filepath.Join(dir, "*.sh"))
	for _, p := range paths {
		name := strings.TrimSuffix(filepath.Base(p), ".sh")
		if exec.Command(p, "enabled").Run() != nil {
			continue
		}
		label, host := name, ""
		if raw, err := exec.Command(p, "info").Output(); err == nil {
			lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
			if len(lines) > 0 && lines[0] != "" {
				label = strings.TrimSpace(lines[0])
			}
			if len(lines) > 1 {
				host = strings.TrimSpace(lines[1])
			}
		}
		out = append(out, engineInfo{Name: name, Label: label, Host: host})
	}
	return out
}

func (s *server) validEngine(name string) bool {
	for _, e := range s.engines {
		if e.Name == name {
			return true
		}
	}
	return false
}

// --- rotas e middlewares ---

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, staticFS, "static/login.html")
	})
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("POST /logout", s.auth(s.handleLogout))
	mux.HandleFunc("GET /{$}", s.auth(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, staticFS, "static/index.html")
	}))
	mux.Handle("GET /static/", http.FileServerFS(staticFS))
	mux.HandleFunc("GET /api/state", s.auth(s.handleState))
	mux.HandleFunc("POST /api/backup", s.auth(s.handleBackup))
	mux.HandleFunc("POST /api/restore", s.auth(s.handleRestore))
	mux.HandleFunc("GET /api/jobs/{id}", s.auth(s.handleJob))
	return s.headers(mux)
}

func (s *server) headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		// same-origin (não no-referrer): a spec do Fetch força Origin: null em
		// requests não-GET/HEAD quando a referrer policy é "no-referrer", o que
		// quebrava a checagem de CSRF em login mesmo em navegação legítima.
		h.Set("Referrer-Policy", "same-origin")
		// 'self' estrito: nada inline, nada externo — todo JS/CSS vem de /static
		h.Set("Content-Security-Policy", "default-src 'self'")
		if s.secureCookies(r) {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("session")
		if err != nil || !verifyToken(s.currentSecret(), c.Value) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				http.Error(w, "não autenticado", http.StatusUnauthorized)
			} else {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
			}
			return
		}
		if r.Method != http.MethodGet && !s.sameOrigin(r) {
			http.Error(w, "origem inválida", http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

// Anti-CSRF (além do SameSite=Strict do cookie): se o navegador mandou
// Origin, ele precisa bater com o Host — ou com PUBLIC_URL, quando configurada.
//
// ponytail: NÃO comparar com X-Forwarded-Host — esse header vem do cliente
// (fetch() pode setá-lo livremente, ao contrário de Origin) e um atacante
// cross-origin o forjaria igual ao próprio Origin dele, driblando a checagem.
// Quando um reverse proxy não repassa o Host original intacto, o admin
// declara o domínio público via PUBLIC_URL (env do container, não do
// cliente) em vez de a gente confiar em qualquer header da requisição.
func (s *server) sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	originHost := u.Hostname()

	reqHost := r.Host
	if h, _, err := net.SplitHostPort(reqHost); err == nil {
		reqHost = h
	}
	if originHost == reqHost {
		return true
	}
	return s.cfg.publicHost != "" && originHost == s.cfg.publicHost
}

func (s *server) currentSecret() []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.secret
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("[Web] erro serializando resposta: %v", err)
	}
}

// --- login/logout ---

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		http.Error(w, "origem inválida", http.StatusForbidden)
		return
	}
	ip := clientIP(r)
	if s.limiter.blocked(ip) {
		http.Redirect(w, r, "/login?e=lock", http.StatusSeeOther)
		return
	}

	userOK := subtle.ConstantTimeCompare([]byte(r.FormValue("user")), []byte(s.cfg.adminUser)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(r.FormValue("password")), []byte(s.cfg.adminPassword)) == 1
	if !userOK || !passOK {
		s.limiter.fail(ip)
		time.Sleep(300 * time.Millisecond) // desacelera força bruta
		http.Redirect(w, r, "/login?e=cred", http.StatusSeeOther)
		return
	}

	s.limiter.success(ip)
	exp := time.Now().Add(s.cfg.sessionTTL)
	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    signToken(s.currentSecret(), r.FormValue("user"), exp),
		Path:     "/",
		Expires:  exp,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   s.secureCookies(r),
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	// O token é stateless: apagar o cookie não basta, um token copiado seguiria
	// válido até expirar. Trocar o segredo revoga todas as sessões.
	// ponytail: com SESSION_SECRET fixo, um restart volta ao segredo original e
	// tokens antigos ainda não expirados revivem; o upgrade seria persistir uma
	// lista de revogação.
	s.mu.Lock()
	s.secret = randomSecret()
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: "session", Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: s.secureCookies(r),
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *server) secureCookies(r *http.Request) bool {
	switch strings.ToLower(s.cfg.cookieSecure) {
	case "true":
		return true
	case "false":
		return false
	default: // auto: TLS direto ou atrás de proxy com X-Forwarded-Proto
		return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
	}
}

// --- estado (servidores, backups, jobs) ---

type engineState struct {
	engineInfo
	Reachable *bool `json:"reachable"` // null = sem host para testar
}

type backupFile struct {
	Engine string    `json:"engine"`
	DB     string    `json:"db"`
	File   string    `json:"file"`
	Size   int64     `json:"size"`
	MTime  time.Time `json:"mtime"`
}

func (s *server) handleState(w http.ResponseWriter, r *http.Request) {
	states := make([]engineState, len(s.engines))
	var wg sync.WaitGroup
	for i, e := range s.engines {
		states[i] = engineState{engineInfo: e}
		if e.Host == "" {
			continue
		}
		wg.Add(1)
		go func(i int, host string) {
			defer wg.Done()
			ok := dialHost(host)
			states[i].Reachable = &ok
		}(i, e.Host)
	}
	wg.Wait()

	writeJSON(w, map[string]any{
		"engines": states,
		"backups": s.listBackups(),
		"jobs":    s.jobs.list(),
		"running": s.jobs.running(),
	})
}

// host pode ser "h:porta", "h1:p1,h2:p2" (replica set) ou "h" sem porta
// (mongodb+srv) — nesse caso resolve o SRV e cai para 27017 se falhar.
func dialHost(host string) bool {
	host = strings.Split(host, ",")[0]
	if !strings.Contains(host, ":") {
		if _, addrs, err := net.LookupSRV("mongodb", "tcp", host); err == nil && len(addrs) > 0 {
			host = strings.TrimSuffix(addrs[0].Target, ".") + ":" + strconv.Itoa(int(addrs[0].Port))
		} else {
			host += ":27017"
		}
	}
	conn, err := net.DialTimeout("tcp", host, 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func (s *server) listBackups() []backupFile {
	out := []backupFile{}
	engines, err := os.ReadDir(s.cfg.backupDir)
	if err != nil {
		return out
	}
	for _, eng := range engines {
		if !eng.IsDir() {
			continue
		}
		dbs, err := os.ReadDir(filepath.Join(s.cfg.backupDir, eng.Name()))
		if err != nil {
			continue
		}
		for _, db := range dbs {
			if !db.IsDir() {
				continue
			}
			files, err := os.ReadDir(filepath.Join(s.cfg.backupDir, eng.Name(), db.Name()))
			if err != nil {
				continue
			}
			for _, f := range files {
				if f.IsDir() {
					continue
				}
				fi, err := f.Info()
				if err != nil {
					continue
				}
				out = append(out, backupFile{
					Engine: eng.Name(), DB: db.Name(), File: f.Name(),
					Size: fi.Size(), MTime: fi.ModTime(),
				})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MTime.After(out[j].MTime) })
	return out
}

// --- ações ---

func (s *server) backupCmd(engine string) *exec.Cmd {
	script := filepath.Join(s.cfg.scriptsDir, "backup.sh")
	if engine != "" {
		return exec.Command(script, engine)
	}
	return exec.Command(script)
}

func (s *server) startJob(w http.ResponseWriter, kind string, cmd *exec.Cmd) {
	j, err := s.jobs.start(kind, cmd)
	if errors.Is(err, errBusy) {
		http.Error(w, "já existe um job em execução", http.StatusConflict)
		return
	}
	if err != nil {
		log.Printf("[Web] falha ao iniciar job %q: %v", kind, err)
		http.Error(w, "falha ao iniciar o job", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]int{"job": j.id})
}

func (s *server) handleBackup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Engine string `json:"engine"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "corpo inválido", http.StatusBadRequest)
		return
	}
	engine := ""
	kind := "backup manual"
	if req.Engine != "" && req.Engine != "all" {
		if !s.validEngine(req.Engine) {
			http.Error(w, "engine desconhecido", http.StatusBadRequest)
			return
		}
		engine = req.Engine
		kind += " (" + engine + ")"
	}
	s.startJob(w, kind, s.backupCmd(engine))
}

var dbNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

func (s *server) handleRestore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Engine string `json:"engine"`
		DB     string `json:"db"`
		File   string `json:"file"`
		Drop   bool   `json:"drop"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "corpo inválido", http.StatusBadRequest)
		return
	}
	if !s.validEngine(req.Engine) || !dbNameRe.MatchString(req.DB) {
		http.Error(w, "engine ou base inválidos", http.StatusBadRequest)
		return
	}
	// fronteira de confiança: o arquivo precisa ser um nome simples e existir
	// dentro de BACKUP_DIR/<engine>/<db> — nada de path traversal
	if req.File == "" || strings.Contains(req.File, "..") || req.File != filepath.Base(req.File) {
		http.Error(w, "arquivo inválido", http.StatusBadRequest)
		return
	}
	path := filepath.Join(s.cfg.backupDir, req.Engine, req.DB, req.File)
	if fi, err := os.Stat(path); err != nil || fi.IsDir() {
		http.Error(w, "backup não encontrado", http.StatusNotFound)
		return
	}

	args := []string{req.Engine, req.DB, path}
	if req.Drop {
		args = append(args, "--drop")
	}
	cmd := exec.Command(filepath.Join(s.cfg.scriptsDir, "restore.sh"), args...)
	s.startJob(w, "restore "+req.Engine+"/"+req.DB, cmd)
}

func (s *server) handleJob(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		http.Error(w, "id inválido", http.StatusBadRequest)
		return
	}
	j := s.jobs.get(id)
	if j == nil {
		http.Error(w, "job não encontrado", http.StatusNotFound)
		return
	}
	writeJSON(w, struct {
		jobView
		Output string `json:"output"`
	}{j.view(), j.output()})
}
