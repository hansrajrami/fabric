package sender

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/hansrajrami/fabric/mst/relay/outbox"
)

// MetricsHandler serves the outbox backlog in Prometheus text exposition
// format (hand-rolled: the format is three trivial line types, not worth a
// client library dependency for a relay daemon). A growing PENDING count or
// oldest-age means MST needs attention — Fabric is unaffected either way.
// balance may be nil; when wired, the relayer's gas balance is exposed as a
// gauge (in gwei — wei magnitudes exceed float64 precision).
func MetricsHandler(store outbox.Store, balance BalanceReader) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stats, err := store.Stats()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var b strings.Builder
		b.WriteString("# HELP mst_outbox_entries Outbox entries by status.\n")
		b.WriteString("# TYPE mst_outbox_entries gauge\n")
		statuses := outbox.AllStatuses()
		sort.Slice(statuses, func(i, j int) bool { return statuses[i] < statuses[j] })
		for _, st := range statuses {
			fmt.Fprintf(&b, "mst_outbox_entries{status=%q} %d\n", st.String(), stats.CountByStatus[st])
		}
		b.WriteString("# HELP mst_outbox_oldest_active_age_seconds Age of the oldest not-yet-DONE entry.\n")
		b.WriteString("# TYPE mst_outbox_oldest_active_age_seconds gauge\n")
		age := int64(0)
		if stats.OldestActiveAge > 0 {
			age = time.Now().Unix() - stats.OldestActiveAge
		}
		fmt.Fprintf(&b, "mst_outbox_oldest_active_age_seconds %d\n", age)
		b.WriteString("# HELP mst_outbox_quarantined_total Malformed proof requests set aside.\n")
		b.WriteString("# TYPE mst_outbox_quarantined_total gauge\n")
		fmt.Fprintf(&b, "mst_outbox_quarantined_total %d\n", stats.Quarantined)

		if balance != nil {
			writeBalanceMetric(&b, r, balance)
		}

		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(b.String()))
	})
}

// BalanceMetricsHandler serves only the relayer balance gauge. Used by the
// embedded peer mode, where one EVM client is shared across several
// per-channel outbox handlers and the gauge must be emitted exactly once.
func BalanceMetricsHandler(balance BalanceReader) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b strings.Builder
		writeBalanceMetric(&b, r, balance)
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(b.String()))
	})
}

func writeBalanceMetric(b *strings.Builder, r *http.Request, balance BalanceReader) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	wei, err := balance.Balance(ctx)
	cancel()
	if err != nil {
		// A scrape must never fail because the RPC hiccuped; the gauge
		// simply goes absent (which alerting can also key on).
		fmt.Fprintf(b, "# mst_relayer_balance_gwei unavailable: %v\n", err)
		return
	}
	b.WriteString("# HELP mst_relayer_balance_gwei Relayer gas account balance (gwei). Fund before it hits zero or anchoring stalls.\n")
	b.WriteString("# TYPE mst_relayer_balance_gwei gauge\n")
	fmt.Fprintf(b, "mst_relayer_balance_gwei %g\n", weiToGwei(wei))
}
