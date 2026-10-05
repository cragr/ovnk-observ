// Command ovnk-observ-exporter exports OVN-Kubernetes scale metrics, either
// per node (OVN NB/SB sockets) or per cluster (Kubernetes informers).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/cragr/ovnk-observ/pkg/k8scount"
	"github.com/cragr/ovnk-observ/pkg/metrics"
	"github.com/cragr/ovnk-observ/pkg/nbcount"
	"github.com/cragr/ovnk-observ/pkg/ovsdbmon"
	"github.com/cragr/ovnk-observ/pkg/pgdrift"
)

// onListen, when set (tests only), receives the bound listen address.
var onListen func(addr string)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("ovnk-observ-exporter", flag.ContinueOnError)
	fs.SetOutput(stdout)
	mode := fs.String("mode", "", "run mode: node or cluster (required)")
	listen := fs.String("listen", ":9410", "listen address")
	netLabels := fs.String("per-network-labels", "topN:50", `network label mode: "off" or "topN:<n>"`)
	nbSock := fs.String("nb-socket", "/var/run/ovn/ovnnb_db.sock", "OVN northbound DB unix socket")
	sbSock := fs.String("sb-socket", "/var/run/ovn/ovnsb_db.sock", "OVN southbound DB unix socket")
	pgDrift := fs.Bool("pg-drift", true, "export NB/SB Port_Group drift metrics (node mode)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *mode != "node" && *mode != "cluster" {
		return fmt.Errorf("invalid --mode %q: want node or cluster", *mode)
	}
	labelMode, err := metrics.ParseNetworkLabelMode(*netLabels)
	if err != nil {
		return err
	}

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}), collectors.NewGoCollector())

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	switch *mode {
	case "node":
		nb, sb := nbcount.NewCounter(), nbcount.NewCounter()
		nbState, sbState := &metrics.DBState{}, &metrics.DBState{}
		reg.MustRegister(metrics.NewNodeCollector(nb, sb, nbState, sbState, labelMode))
		nbTables, sbTables := ovsdbmon.NBTables, ovsdbmon.SBTables
		var nbHook, sbHook ovsdbmon.RowHook
		if *pgDrift {
			tr := pgdrift.NewTracker()
			nbTables = ovsdbmon.WithColumns(ovsdbmon.NBTables, "Port_Group", "name", "ports")
			sbTables = ovsdbmon.WithColumns(ovsdbmon.SBTables, "Port_Group", "name")
			nbHook, sbHook = tr.NB(), tr.SB()
			reg.MustRegister(metrics.NewDriftCollector(tr, nbState, sbState))
		}
		start := func(name, db, sock string, tables []ovsdbmon.TableSpec, c *nbcount.Counter, st *metrics.DBState, hook ovsdbmon.RowHook) {
			go func() {
				_ = ovsdbmon.Run(ctx, ovsdbmon.Config{
					Socket: sock, Database: db, Tables: tables, Counter: c, Hook: hook,
					OnState: func(connected bool, d time.Duration) { st.Set(connected, d) },
				})
			}()
		}
		start("nb", "OVN_Northbound", *nbSock, nbTables, nb, nbState, nbHook)
		start("sb", "OVN_Southbound", *sbSock, sbTables, sb, sbState, sbHook)
	case "cluster":
		cfg, err := rest.InClusterConfig()
		if err != nil {
			return fmt.Errorf("in-cluster config: %w", err)
		}
		dyn, err := dynamic.NewForConfig(cfg)
		if err != nil {
			return err
		}
		kube, err := kubernetes.NewForConfig(cfg)
		if err != nil {
			return err
		}
		col, err := k8scount.NewCollector(ctx, dyn, kube)
		if err != nil {
			return err
		}
		reg.MustRegister(col)
	}

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok\n")
	})

	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	slog.Info("starting exporter", "mode", *mode, "listen", ln.Addr().String(),
		"nb_socket", *nbSock, "sb_socket", *sbSock, "per_network_labels", *netLabels, "pg_drift", *pgDrift)
	if onListen != nil {
		onListen(ln.Addr().String())
	}

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	shCtx, shCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shCancel()
	if err := srv.Shutdown(shCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
