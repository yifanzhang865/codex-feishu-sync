package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/zhangwei/codex-feishu-sync/internal/appserver"
	"github.com/zhangwei/codex-feishu-sync/internal/control"
)

// Opt-in compatibility test: real native TUI and real App Server, isolated
// history, native resume picker, real bridge callback, no model turn. Standard
// tests use deterministic RPC fixtures for takeover, approval invalidation,
// and disconnect races.
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
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	b, _, _ := observerFixture(t)
	b.ctx = ctx
	b.cfg.AutoCreateGroup = false
	client, err := appserver.Start(ctx, executable, b.onNotification, b.onServerRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	b.codex = client
	var mu sync.Mutex
	var methods []string
	var bootstrapError error
	var requirementsRead, pickerListed bool
	b.localControl = control.New(ctx, client, control.Hooks{Call: func(ctx context.Context, owner, method string, raw json.RawMessage) (json.RawMessage, error) {
		result, err := b.onLocalCLICall(ctx, owner, method, raw)
		mu.Lock()
		methods = append(methods, method)
		if method == "configRequirements/read" {
			requirementsRead = err == nil
			bootstrapError = err
		}
		if method == "thread/list" && err == nil {
			pickerListed = true
		}
		mu.Unlock()
		return result, err
	}, Takeover: b.onControlTakeover, FeishuRequest: b.onFeishuServerRequest})
	client.SetNotificationTap(b.localControl.Notify)
	if err := b.localControl.Start(dir); err != nil {
		t.Fatal(err)
	}
	defer b.localControl.Close()
	endpoint, err := control.LoadEndpoint(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Verify the exact parameterless bootstrap request against a real backend
	// even when an empty resume picker does not open a conversation yet.
	conn, response, err := websocket.DefaultDialer.Dial(endpoint.URL, http.Header{"Authorization": {"Bearer " + endpoint.Token}})
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for _, request := range []map[string]any{
		{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]string{"name": "codex_cli_rs"}}},
		{"id": 2, "method": "configRequirements/read"},
	} {
		if err := conn.WriteJSON(request); err != nil {
			_ = conn.Close()
			t.Fatal(err)
		}
		var reply struct {
			Result json.RawMessage     `json:"result"`
			Error  *appserver.RPCError `json:"error"`
		}
		if err := conn.ReadJSON(&reply); err != nil {
			_ = conn.Close()
			t.Fatal(err)
		}
		if reply.Error != nil || !json.Valid(reply.Result) {
			_ = conn.Close()
			t.Fatalf("real bootstrap probe failed: %v", reply.Error)
		}
	}
	_ = conn.Close()
	// Reply to cursor-position queries and retain only redacted error diagnostics.
	script := `import os,pty,select,signal,struct,fcntl,termios,time,re
pid,fd=pty.fork()
if pid==0:
 os.execv(os.environ['TEST_CODEX'],[os.environ['TEST_CODEX'],'--remote',os.environ['TEST_ENDPOINT'],'--remote-auth-token-env','CODEX_FEISHU_CONTROL_TOKEN','--no-alt-screen','-C',os.environ['CODEX_HOME'],'resume','--all'])
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
try: os.killpg(pid,signal.SIGTERM)
except ProcessLookupError: pass
stop=time.monotonic()+2
while time.monotonic()<stop:
 try:
  done,_=os.waitpid(pid,os.WNOHANG)
  if done: break
 except ChildProcessError: break
 time.sleep(0.05)
else:
 try: os.killpg(pid,signal.SIGKILL)
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
	command.Env = append(os.Environ(), "TEST_CODEX="+executable, "TEST_ENDPOINT="+endpoint.URL, "CODEX_FEISHU_CONTROL_TOKEN="+endpoint.Token, "TERM=xterm-256color", "NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost")
	diagnostic, err := command.CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if bootstrapError != nil || !requirementsRead || !pickerListed {
		t.Fatalf("native resume bootstrap failed: requirementsRead=%v pickerListed=%v error=%v methods=%v diagnostic=%s", requirementsRead, pickerListed, bootstrapError, methods, diagnostic)
	}
	t.Log("native resume picker queried history through the complete bridge; configRequirements/read succeeded")
	for _, method := range methods {
		if method == "turn/start" || method == "turn/steer" {
			t.Fatal("compatibility test unexpectedly submitted a model turn")
		}
	}
}
