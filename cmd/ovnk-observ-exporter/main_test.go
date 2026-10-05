package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestRunRejectsBadMode(t *testing.T) {
	err := run(context.Background(), []string{"--mode=foo"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "mode") {
		t.Fatalf("want mode error, got %v", err)
	}
}

func TestRunRejectsBadNetworkLabels(t *testing.T) {
	err := run(context.Background(), []string{"--mode=node", "--per-network-labels=top:5"}, io.Discard)
	if err == nil {
		t.Fatal("want error")
	}
}

func TestRunRejectsBadIncEngineNodes(t *testing.T) {
	for _, v := range []string{"", " , "} {
		err := run(context.Background(), []string{"--mode=node", "--inc-engine-nodes=" + v}, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "inc-engine-nodes") {
			t.Fatalf("nodes %q: want inc-engine-nodes error, got %v", v, err)
		}
	}
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestNodeModeServesMetricsWithoutSockets(t *testing.T) {
	addrCh := make(chan string, 1)
	onListen = func(a string) { addrCh <- a }
	defer func() { onListen = nil }()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"--mode=node", "--listen=127.0.0.1:0",
			"--nb-socket=/nonexistent/nb.sock", "--sb-socket=/nonexistent/sb.sock"}, io.Discard)
	}()
	var addr string
	select {
	case addr = <-addrCh:
	case err := <-done:
		t.Fatalf("run exited early: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for listen")
	}
	if code, _ := get(t, "http://"+addr+"/healthz"); code != 200 {
		t.Fatalf("healthz = %d", code)
	}
	code, body := get(t, "http://"+addr+"/metrics")
	if code != 200 || !strings.Contains(body, `ovnk_observ_db_connected{db="nb"} 0`) {
		t.Fatalf("metrics (%d) missing connected gauge:\n%s", code, body)
	}
	if !strings.Contains(body, `ovnk_observ_appctl_errors_total{command="inc-engine/show-stats"}`) {
		t.Fatalf("metrics missing appctl errors counter:\n%s", body)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown err: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no shutdown")
	}
}

func TestNodeModePGDriftFlag(t *testing.T) {
	addrCh := make(chan string, 1)
	onListen = func(a string) { addrCh <- a }
	defer func() { onListen = nil }()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"--mode=node", "--pg-drift=false", "--listen=127.0.0.1:0",
			"--nb-socket=/nonexistent/nb.sock", "--sb-socket=/nonexistent/sb.sock"}, io.Discard)
	}()
	var addr string
	select {
	case addr = <-addrCh:
	case err := <-done:
		t.Fatalf("run exited early: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for listen")
	}
	code, body := get(t, "http://"+addr+"/metrics")
	if code != 200 || !strings.Contains(body, `ovnk_observ_db_connected{db="nb"} 0`) {
		t.Fatalf("metrics (%d) missing connected gauge:\n%s", code, body)
	}
	if strings.Contains(body, "port_group_sb_missing") {
		t.Fatalf("drift series present with --pg-drift=false:\n%s", body)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown err: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no shutdown")
	}
}

func TestRunRejectsBadChurnManagerLabel(t *testing.T) {
	err := run(context.Background(), []string{"--mode=cluster", "--churn-manager-label=top:5"}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "churn-manager-label") {
		t.Fatalf("want churn-manager-label error, got %v", err)
	}
}
