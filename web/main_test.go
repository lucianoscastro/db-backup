package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testServer(t *testing.T) *server {
	t.Helper()
	dir := t.TempDir()

	// driver fake: "enabled" responde 0, "info" imprime rótulo e host
	driversDir := filepath.Join(dir, "drivers")
	if err := os.MkdirAll(driversDir, 0o755); err != nil {
		t.Fatal(err)
	}
	driver := "#!/bin/sh\ncase \"$1\" in\n  enabled) exit 0 ;;\n  info) echo Fake; echo localhost:1 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(driversDir, "mongo.sh"), []byte(driver), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := config{
		adminUser:     "admin",
		adminPassword: "senha-teste-123",
		sessionTTL:    time.Hour,
		cookieSecure:  "false",
		backupDir:     dir,
		driversDir:    driversDir,
		scriptsDir:    dir,
	}
	return newServer(cfg, []byte("segredo-de-teste"), newJobManager())
}

func login(t *testing.T, h http.Handler, user, pass string) *http.Response {
	t.Helper()
	form := url.Values{"user": {user}, "password": {pass}}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "192.0.2.1:4321"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Result()
}

func sessionCookie(t *testing.T, res *http.Response) *http.Cookie {
	t.Helper()
	for _, c := range res.Cookies() {
		if c.Name == "session" && c.Value != "" {
			return c
		}
	}
	return nil
}

func TestUnauthenticatedIsRejected(t *testing.T) {
	h := testServer(t).routes()

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/login" {
		t.Fatalf("GET / sem sessão: esperado redirect para /login, veio %d %s", w.Code, w.Header().Get("Location"))
	}

	req = httptest.NewRequest("GET", "/api/state", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("GET /api/state sem sessão: esperado 401, veio %d", w.Code)
	}
}

func TestLoginWrongPassword(t *testing.T) {
	h := testServer(t).routes()
	res := login(t, h, "admin", "senha-errada")
	if loc := res.Header.Get("Location"); loc != "/login?e=cred" {
		t.Fatalf("esperado redirect para /login?e=cred, veio %q", loc)
	}
	if sessionCookie(t, res) != nil {
		t.Fatal("login inválido não pode emitir cookie de sessão")
	}
}

func TestLoginAndAuthenticatedState(t *testing.T) {
	h := testServer(t).routes()
	res := login(t, h, "admin", "senha-teste-123")
	c := sessionCookie(t, res)
	if res.Header.Get("Location") != "/" || c == nil {
		t.Fatalf("login válido: esperado redirect para / com cookie, veio %q", res.Header.Get("Location"))
	}
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
		t.Fatal("cookie de sessão precisa ser HttpOnly + SameSite=Strict")
	}

	req := httptest.NewRequest("GET", "/api/state", nil)
	req.AddCookie(c)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /api/state autenticado: esperado 200, veio %d (%s)", w.Code, w.Body.String())
	}
	var state struct {
		Engines []engineState `json:"engines"`
	}
	if err := json.NewDecoder(w.Body).Decode(&state); err != nil {
		t.Fatalf("resposta não é JSON válido: %v", err)
	}
	if len(state.Engines) != 1 || state.Engines[0].Label != "Fake" {
		t.Fatalf("esperado 1 engine 'Fake' descoberto via driver, veio %+v", state.Engines)
	}
}

func TestTamperedTokenIsRejected(t *testing.T) {
	s := testServer(t)
	h := s.routes()
	token := signToken(s.secret, "admin", time.Now().Add(time.Hour))
	tampered := token[:len(token)-2] + "xx"

	req := httptest.NewRequest("GET", "/api/state", nil)
	req.AddCookie(&http.Cookie{Name: "session", Value: tampered})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("token adulterado: esperado 401, veio %d", w.Code)
	}

	if verifyToken(s.secret, signToken(s.secret, "admin", time.Now().Add(-time.Minute))) {
		t.Fatal("token expirado não pode validar")
	}
}

func TestLoginRateLimit(t *testing.T) {
	h := testServer(t).routes()
	for range maxLoginFails {
		login(t, h, "admin", "senha-errada")
	}
	// bloqueado: nem a senha correta entra
	res := login(t, h, "admin", "senha-teste-123")
	if loc := res.Header.Get("Location"); loc != "/login?e=lock" {
		t.Fatalf("esperado bloqueio (/login?e=lock), veio %q", loc)
	}
}

func TestCrossOriginPostIsRejected(t *testing.T) {
	s := testServer(t)
	h := s.routes()
	token := signToken(s.secret, "admin", time.Now().Add(time.Hour))

	req := httptest.NewRequest("POST", "/api/backup", strings.NewReader(`{"engine":"all"}`))
	req.AddCookie(&http.Cookie{Name: "session", Value: token})
	req.Header.Set("Origin", "https://malicioso.example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("POST cross-origin: esperado 403, veio %d", w.Code)
	}
}

// Regressão: sameOrigin não pode confiar em X-Forwarded-Host. Diferente de
// Origin (que o navegador define e o JS não sobrescreve), um fetch()
// cross-origin pode setar esse header livremente — se ele fosse comparado
// contra o Origin, um atacante forjaria os dois iguais e passaria pela
// checagem de CSRF.
func TestCrossOriginPostIgnoresForwardedHostSpoof(t *testing.T) {
	s := testServer(t)
	h := s.routes()
	token := signToken(s.secret, "admin", time.Now().Add(time.Hour))

	req := httptest.NewRequest("POST", "/api/backup", strings.NewReader(`{"engine":"all"}`))
	req.AddCookie(&http.Cookie{Name: "session", Value: token})
	req.Header.Set("Origin", "https://malicioso.example.com")
	req.Header.Set("X-Forwarded-Host", "malicioso.example.com")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("POST cross-origin com X-Forwarded-Host forjado: esperado 403, veio %d", w.Code)
	}
}

// Regressão: em produção, um reverse proxy que não repassa o Host original
// intacto (ex.: nginx sem `proxy_set_header Host $host;`) faz r.Host chegar
// diferente do Origin que o navegador manda, e a checagem de CSRF rejeitava
// um login legítimo com "origem inválida". PUBLIC_URL existe para o admin
// declarar o domínio público sem reabrir a brecha do X-Forwarded-Host.
func TestPublicURLAcceptsConfiguredOrigin(t *testing.T) {
	s := testServer(t)
	s.cfg.publicHost = "backup.lzc.tec.br"
	h := s.routes()
	token := signToken(s.secret, "admin", time.Now().Add(time.Hour))

	req := httptest.NewRequest("POST", "/api/backup", strings.NewReader(`{"engine":"all"}`))
	req.Host = "internal-service:8080" // Host visto pelo Go atrás do proxy mal configurado
	req.AddCookie(&http.Cookie{Name: "session", Value: token})
	req.Header.Set("Origin", "https://backup.lzc.tec.br")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code == http.StatusForbidden {
		t.Fatalf("Origin igual a PUBLIC_URL deveria passar mesmo com Host diferente, veio 403")
	}

	// Uma origem que não é nem o Host nem o PUBLIC_URL continua bloqueada.
	req2 := httptest.NewRequest("POST", "/api/backup", strings.NewReader(`{"engine":"all"}`))
	req2.Host = "internal-service:8080"
	req2.AddCookie(&http.Cookie{Name: "session", Value: token})
	req2.Header.Set("Origin", "https://malicioso.example.com")
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)
	if w2.Code != http.StatusForbidden {
		t.Fatalf("Origin fora de Host e PUBLIC_URL: esperado 403, veio %d", w2.Code)
	}
}

func TestRestoreRejectsTraversalAndUnknowns(t *testing.T) {
	s := testServer(t)
	h := s.routes()
	token := signToken(s.secret, "admin", time.Now().Add(time.Hour))

	cases := []struct {
		name, body string
		want       int
	}{
		{"path traversal no arquivo", `{"engine":"mongo","db":"loja","file":"../../etc/passwd"}`, http.StatusBadRequest},
		{"traversal no nome da base", `{"engine":"mongo","db":"../etc","file":"x.tar"}`, http.StatusBadRequest},
		{"engine desconhecido", `{"engine":"pg","db":"loja","file":"x.tar"}`, http.StatusBadRequest},
		{"arquivo inexistente", `{"engine":"mongo","db":"loja","file":"nao-existe.tar"}`, http.StatusNotFound},
	}
	for _, tc := range cases {
		req := httptest.NewRequest("POST", "/api/restore", strings.NewReader(tc.body))
		req.AddCookie(&http.Cookie{Name: "session", Value: token})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != tc.want {
			t.Errorf("%s: esperado %d, veio %d", tc.name, tc.want, w.Code)
		}
	}
}

func TestLogoutRevokesCopiedToken(t *testing.T) {
	h := testServer(t).routes()
	c := sessionCookie(t, login(t, h, "admin", "senha-teste-123"))

	req := httptest.NewRequest("POST", "/logout", nil)
	req.AddCookie(c)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("logout: esperado 303, veio %d", w.Code)
	}

	req = httptest.NewRequest("GET", "/api/state", nil)
	req.AddCookie(c)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("token copiado após logout: esperado 401, veio %d", w.Code)
	}
}
