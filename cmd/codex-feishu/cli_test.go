package main

import (
	"strings"
	"testing"
)

func TestCLICannotBypassLocalController(t *testing.T) {
	for _, arg := range []string{"--remote", "--remote=ws://other", "--remote-auth-token-env", "--remote-auth-token-env=OTHER_TOKEN", "--no-daemon"} {
		if err := runCLI([]string{arg}); err == nil || !strings.Contains(err.Error(), "自动使用本机控制入口") {
			t.Fatalf("accepted controller bypass %q: %v", arg, err)
		}
	}
}

func TestLocalControllerBypassesProxyAndReplacesStaleToken(t *testing.T) {
	got := controlEnvironment([]string{"HTTP_PROXY=http://proxy.invalid:8080", "NO_PROXY=internal.example", "no_proxy=another.example", "CODEX_FEISHU_CONTROL_TOKEN=stale", "PATH=/bin"}, "fresh")
	joined := strings.Join(got, "\n")
	if strings.Contains(joined, "stale") || !strings.Contains(joined, "CODEX_FEISHU_CONTROL_TOKEN=fresh") {
		t.Fatal("stale control token inherited")
	}
	if !strings.Contains(joined, "HTTP_PROXY=http://proxy.invalid:8080") || !strings.Contains(joined, "PATH=/bin") {
		t.Fatal("unrelated environment changed")
	}
	for _, key := range []string{"NO_PROXY=", "no_proxy="} {
		if !strings.Contains(joined, key+"internal.example,another.example,127.0.0.1,localhost") {
			t.Fatal("proxy bypass list lost local or existing hosts")
		}
	}
}
