// Package appctl is a minimal, read-only client for OVS unixctl sockets. It
// can send exactly one command, inc-engine/show-stats, to ovn-northd.
package appctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// showStatsCmd is the only method this package ever sends.
const showStatsCmd = "inc-engine/show-stats"

// ShowStatsCommand is exported only as the value of the metric "command"
// label. Nothing accepts it as input.
const ShowStatsCommand = showStatsCmd

// maxReply bounds how much of a reply is read.
const maxReply = 1 << 20

// FindCtl returns the control socket path <runDir>/<target>.<pid>.ctl, with
// the pid read from <runDir>/<target>.pid.
func FindCtl(runDir, target string) (string, error) {
	b, err := os.ReadFile(filepath.Join(runDir, target+".pid"))
	if err != nil {
		return "", fmt.Errorf("read pid file for %s: %w", target, err)
	}
	pid := strings.TrimSpace(string(b))
	if _, err := strconv.Atoi(pid); err != nil {
		return "", fmt.Errorf("pid file for %s: invalid pid %q", target, pid)
	}
	return filepath.Join(runDir, target+"."+pid+".ctl"), nil
}

type request struct {
	ID     int      `json:"id"`
	Method string   `json:"method"`
	Params []string `json:"params"`
}

type reply struct {
	Result *string         `json:"result"`
	Error  json.RawMessage `json:"error"`
}

// ShowIncEngineStats runs inc-engine/show-stats against ovn-northd and
// returns the text result. The socket is located afresh on every call.
func ShowIncEngineStats(ctx context.Context, runDir string) (string, error) {
	path, err := FindCtl(runDir, "ovn-northd")
	if err != nil {
		return "", err
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return "", wrapCtx(ctx, fmt.Errorf("dial %s: %w", path, err))
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	req := request{ID: 0, Method: showStatsCmd, Params: []string{}}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return "", wrapCtx(ctx, fmt.Errorf("send %s: %w", showStatsCmd, err))
	}
	var rep reply
	if err := json.NewDecoder(io.LimitReader(conn, maxReply)).Decode(&rep); err != nil {
		return "", wrapCtx(ctx, fmt.Errorf("read %s reply: %w", showStatsCmd, err))
	}
	if len(rep.Error) > 0 && string(rep.Error) != "null" {
		var s string
		if json.Unmarshal(rep.Error, &s) != nil {
			s = string(rep.Error)
		}
		return "", fmt.Errorf("%s: %s", showStatsCmd, s)
	}
	if rep.Result == nil {
		return "", fmt.Errorf("%s: reply has no result", showStatsCmd)
	}
	return *rep.Result, nil
}

// wrapCtx returns the context error (wrapped) when ctx is done, so callers
// can use errors.Is(err, context.DeadlineExceeded).
func wrapCtx(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return fmt.Errorf("%w: %v", cerr, err)
	}
	// The conn deadline can fire a moment before ctx.Err() is set.
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return fmt.Errorf("%w: %v", context.DeadlineExceeded, err)
	}
	return err
}
