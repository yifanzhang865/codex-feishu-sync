package control

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/zhangwei/codex-feishu-sync/internal/appserver"
)

// Opt-in compatibility test: real native TUI and real App Server, isolated
// history, no prompt and no model turn. Standard tests use deterministic RPC
// fixtures for takeover, approval invalidation, and disconnect races.
func TestNativeRemoteCLIStartup(t *testing.T) {
	if os.Getenv("CODEX_FEISHU_NATIVE_TEST") != "1" {
		t.Skip("set CODEX_FEISHU_NATIVE_TEST=1 to verify an installed native CLI")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is needed for a PTY")
	}
	executable, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal(err)
	}
	oldHome := os.Getenv("CODEX_HOME")
	if oldHome == "" {
		home, _ := os.UserHomeDir()
		oldHome = filepath.Join(home, ".codex")
	}
	auth, err := os.ReadFile(filepath.Join(oldHome, "auth.json"))
	if err != nil {
		t.Skip("native TUI startup needs an existing Codex login")
	}
	dir := t.TempDir()
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(previousLogger)
	t.Setenv("CODEX_HOME", dir)
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), auth, 0600); err != nil {
		t.Fatal(err)
	}
	if cache, err := os.ReadFile(filepath.Join(oldHome, "models_cache.json")); err == nil {
		if err := os.WriteFile(filepath.Join(dir, "models_cache.json"), cache, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[projects."+strconv.Quote(dir)+"]\ntrust_level = \"trusted\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	var server *Server
	client, err := appserver.Start(ctx, executable, func(string, json.RawMessage) {}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var mu sync.Mutex
	var methods []string
	var created bool
	server = New(ctx, client, Hooks{Call: func(ctx context.Context, owner, method string, raw json.RawMessage) (json.RawMessage, error) {
		mu.Lock()
		methods = append(methods, method)
		mu.Unlock()
		result, err := server.Execute(ctx, owner, method, raw)
		if err == nil && method == "thread/start" {
			var response struct {
				Thread appserver.Thread `json:"thread"`
			}
			_ = json.Unmarshal(result, &response)
			server.Manage(response.Thread)
			mu.Lock()
			created = response.Thread.ID != ""
			mu.Unlock()
		}
		return result, err
	}})
	client.SetNotificationTap(server.Notify)
	if err := server.Start(dir); err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	// Terminal replies are needed for native TUI cursor-position detection.
	// Captured output is deliberately discarded, so auth data cannot enter logs.
	script := `import os,pty,select,signal,struct,fcntl,termios,time,re
pid,fd=pty.fork()
if pid==0:
 os.execv(os.environ['TEST_CODEX'],[os.environ['TEST_CODEX'],'--remote',os.environ['TEST_ENDPOINT'],'--remote-auth-token-env','CODEX_FEISHU_CONTROL_TOKEN','--no-alt-screen','-C',os.environ['CODEX_HOME']])
fcntl.ioctl(fd,termios.TIOCSWINSZ,struct.pack('HHHH',32,120,0,0))
end=time.monotonic()+12
output=bytearray()
while time.monotonic()<end:
 if select.select([fd],[],[],0.15)[0]:
  try: data=os.read(fd,65536)
  except OSError: break
  output.extend(data)
  if b'\x1b[6n' in data: os.write(fd,b'\x1b[1;1R')
 try:
  done,_=os.waitpid(pid,os.WNOHANG)
  if done: break
 except ChildProcessError: break
try: os.kill(pid,signal.SIGTERM)
except ProcessLookupError: pass
try: os.waitpid(pid,0)
except ChildProcessError: pass
os.close(fd)
text=output.decode('utf-8','replace').replace(os.environ['CODEX_FEISHU_CONTROL_TOKEN'],'[redacted]')
text=re.sub(r'\x1b\[[0-9;?]*[A-Za-z]','',text)
text=re.sub(r'(?:https?|wss?)://\S+','[url]',text)
for match in re.findall(r'(?im)(?:error|failed|unsupported|invalid|could not)[^\r\n]{0,350}',text)[-3:]:
 print(match)
`
	command := exec.CommandContext(ctx, "python3", "-c", script)
	command.Env = append(os.Environ(), "TEST_CODEX="+executable, "TEST_ENDPOINT="+server.endpoint.URL, "CODEX_FEISHU_CONTROL_TOKEN="+server.endpoint.Token, "TERM=xterm-256color", "NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost")
	diagnostic, err := command.CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	// A fresh model catalog can depend on network access. The assertion checks
	// native initialization and real RPC exchange; thread creation is recorded
	// when the catalog is ready, without requiring a cloud endpoint in this test.
	connected := false
	for _, method := range methods {
		if method == "config/read" {
			connected = true
		}
	}
	if !connected {
		t.Fatalf("native TUI did not connect; methods=%v; diagnostic=%s", methods, diagnostic)
	}
	t.Logf("native RPC connected; thread opened=%v", created)
	for _, method := range methods {
		if method == "turn/start" || method == "turn/steer" {
			t.Fatal("compatibility test unexpectedly submitted a model turn")
		}
	}
}
