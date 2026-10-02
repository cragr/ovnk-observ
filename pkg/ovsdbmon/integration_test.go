//go:build integration

package ovsdbmon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cragr/ovnk-observ/pkg/nbcount"
)

const (
	itSchema   = "/usr/share/ovn/ovn-nb.ovsschema"
	itACLs     = 1000
	itPGs      = 500
	itDelACLs  = 10
	itOwnerKey = `external_ids:"k8s.ovn.org/owner-controller"=net1-network-controller`
	itTypeKey  = `external_ids:"k8s.ovn.org/owner-type"=NetworkPolicy`
)

func itRun(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s failed: %v\n%.2000s", name, err, out)
	}
	return string(out)
}

// itNbctl runs ovn-nbctl against sock, joining cmds (each a []string) with "--".
func itNbctl(t *testing.T, sock string, cmds ...[]string) {
	t.Helper()
	args := []string{"--db=unix:" + sock}
	for _, c := range cmds {
		args = append(args, "--")
		args = append(args, c...)
	}
	itRun(t, "ovn-nbctl", args...)
}

func itWait(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", d, what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func TestMonitorAgainstRealOVSDBServer(t *testing.T) {
	for _, b := range []string{"ovsdb-server", "ovsdb-tool", "ovn-nbctl"} {
		if _, err := exec.LookPath(b); err != nil {
			t.Skipf("%s not in PATH", b)
		}
	}
	dir := t.TempDir()
	dbFile := filepath.Join(dir, "nb.db")
	sock := filepath.Join(dir, "nb.sock")
	itRun(t, "ovsdb-tool", "create", dbFile, itSchema)

	srv := exec.Command("ovsdb-server", "--remote=punix:"+sock,
		"--unixctl="+filepath.Join(dir, "ctl"), "--pidfile="+filepath.Join(dir, "pid"), dbFile)
	srv.Stdout, srv.Stderr = os.Stderr, os.Stderr
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	var killOnce sync.Once
	kill := func() {
		killOnce.Do(func() {
			_ = srv.Process.Kill()
			_ = srv.Wait()
		})
	}
	t.Cleanup(kill)
	itWait(t, 10*time.Second, "ovsdb-server socket", func() bool { _, err := os.Stat(sock); return err == nil })

	// 1,000 ACLs on one switch, 500 Port_Groups, batched into few invocations.
	cmds := [][]string{{"ls-add", "ls1"}}
	for i := 0; i < itACLs; i++ {
		id := fmt.Sprintf("@a%d", i)
		cmds = append(cmds,
			[]string{"--id=" + id, "create", "ACL", "direction=to-lport", fmt.Sprintf("priority=%d", 1000+i),
				fmt.Sprintf("match=\"tcp.dst == %d\"", i), "action=allow", itOwnerKey, itTypeKey},
			[]string{"add", "Logical_Switch", "ls1", "acls", id})
	}
	itNbctl(t, sock, cmds...)
	for start := 0; start < itPGs; start += 250 {
		var pgs [][]string
		for i := start; i < start+250; i++ {
			pgs = append(pgs, []string{"create", "Port_Group", fmt.Sprintf("name=pg%d", i), itOwnerKey, itTypeKey})
		}
		itNbctl(t, sock, pgs...)
	}

	counter := nbcount.NewCounter()
	var mu sync.Mutex
	var states []bool
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = Run(ctx, Config{
			Socket: sock, Database: "OVN_Northbound", Tables: NBTables, Counter: counter,
			OnState: func(c bool, _ time.Duration) { mu.Lock(); states = append(states, c); mu.Unlock() },
		})
	}()
	t.Cleanup(func() { cancel(); <-done })

	acl := nbcount.Key{OwnerType: "NetworkPolicy", Network: "net1"}
	aclCount := func() int { return counter.Snapshot()["ACL"][acl] }
	pgTotal := func() int {
		n := 0
		for _, v := range counter.Snapshot()["Port_Group"] {
			n += v
		}
		return n
	}
	itWait(t, 10*time.Second, "initial sync", func() bool { return aclCount() == itACLs && pgTotal() == itPGs })
	if got := counter.Snapshot()["Port_Group"][acl]; got != itPGs {
		t.Errorf("Port_Group{NetworkPolicy,net1} = %d, want %d", got, itPGs)
	}

	var dels [][]string
	for i := 0; i < itDelACLs; i++ {
		dels = append(dels, []string{"acl-del", "ls1", "to-lport", fmt.Sprintf("%d", 1000+i), fmt.Sprintf("tcp.dst == %d", i)})
	}
	itNbctl(t, sock, dels...)
	itWait(t, 5*time.Second, "ACL count 990", func() bool { return aclCount() == itACLs-itDelACLs })
	itWait(t, 5*time.Second, "ACL delete updates", func() bool {
		return counter.Updates()[[2]string{"ACL", "delete"}] == itDelACLs
	})
	if got := counter.Updates()[[2]string{"ACL", "delete"}]; got != itDelACLs {
		t.Errorf("Updates[ACL,delete] = %d, want %d", got, itDelACLs)
	}

	kill()
	itWait(t, 5*time.Second, "OnState(false) after server kill", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(states) > 0 && !states[len(states)-1] && strings.Contains(fmt.Sprint(states), "true")
	})
}
