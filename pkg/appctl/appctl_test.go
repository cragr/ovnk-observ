package appctl

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tempRunDir returns a short temp dir; macOS limits unix socket paths to ~104 bytes.
func tempRunDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("", "actl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func writePID(t *testing.T, dir, pid string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "ovn-northd.pid"), []byte(pid+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// serve listens on ovn-northd.<pid>.ctl and handles each connection with h.
func serve(t *testing.T, dir, pid string, h func(net.Conn)) net.Listener {
	t.Helper()
	l, err := net.Listen("unix", filepath.Join(dir, "ovn-northd."+pid+".ctl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				h(c)
			}()
		}
	}()
	return l
}

func replyWith(reply string, got chan<- map[string]any) func(net.Conn) {
	return func(c net.Conn) {
		var req map[string]any
		if err := json.NewDecoder(c).Decode(&req); err != nil {
			return
		}
		if got != nil {
			got <- req
		}
		c.Write([]byte(reply))
	}
}

func TestShowStatsSendsOnlyShowStats(t *testing.T) {
	dir := tempRunDir(t)
	writePID(t, dir, "100")
	got := make(chan map[string]any, 1)
	serve(t, dir, "100", replyWith(`{"id":0,"result":"Node: northd\n- recompute: 1\n","error":null}`, got))

	out, err := ShowIncEngineStats(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if out != "Node: northd\n- recompute: 1\n" {
		t.Fatalf("unexpected result %q", out)
	}
	req := <-got
	if req["method"] != "inc-engine/show-stats" {
		t.Fatalf("method = %v", req["method"])
	}
	if p, ok := req["params"].([]any); !ok || len(p) != 0 {
		t.Fatalf("params = %v", req["params"])
	}
}

func TestShowStatsErrorReply(t *testing.T) {
	dir := tempRunDir(t)
	writePID(t, dir, "100")
	serve(t, dir, "100", replyWith(`{"id":0,"result":null,"error":"\"inc-engine/show-stats\" is not a valid command"}`, nil))

	_, err := ShowIncEngineStats(context.Background(), dir)
	if err == nil || !strings.Contains(err.Error(), "not a valid command") {
		t.Fatalf("err = %v", err)
	}
}

func TestShowStatsFollowsPIDChange(t *testing.T) {
	dir := tempRunDir(t)
	writePID(t, dir, "100")
	l1 := serve(t, dir, "100", replyWith(`{"id":0,"result":"one","error":null}`, nil))
	if out, err := ShowIncEngineStats(context.Background(), dir); err != nil || out != "one" {
		t.Fatalf("first: %q %v", out, err)
	}
	writePID(t, dir, "200")
	serve(t, dir, "200", replyWith(`{"id":0,"result":"two","error":null}`, nil))
	l1.Close()
	if out, err := ShowIncEngineStats(context.Background(), dir); err != nil || out != "two" {
		t.Fatalf("second: %q %v", out, err)
	}
}

func TestShowStatsTimeout(t *testing.T) {
	dir := tempRunDir(t)
	writePID(t, dir, "100")
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	serve(t, dir, "100", func(c net.Conn) { <-done })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := ShowIncEngineStats(ctx, dir)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
}

func TestFindCtlBadPID(t *testing.T) {
	dir := tempRunDir(t)
	writePID(t, dir, "abc")
	if _, err := FindCtl(dir, "ovn-northd"); err == nil {
		t.Fatal("expected error")
	}
	if _, err := FindCtl(dir, "missing"); err == nil {
		t.Fatal("expected error for missing pid file")
	}
}
