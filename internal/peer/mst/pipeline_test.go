/*
Copyright the MST integration contributors.
SPDX-License-Identifier: Apache-2.0
*/

package mst

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
)

func TestPipelineHappyPath(t *testing.T) {
	t.Cleanup(func() {
		viper.Reset()
		resetFlags()
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			w.Write([]byte("ok\n"))
		case "/metrics":
			w.Write([]byte("# channel mychannel\n" +
				`mst_outbox_entries{status="PENDING"} 1` + "\n" +
				`mst_outbox_entries{status="DONE"} 9` + "\n" +
				"# relayer account\nmst_relayer_balance_gwei 500000\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	viper.Set("mst.metricsAddr", strings.TrimPrefix(srv.URL, "http://"))

	// pipeline uses no endorser; the fake is unused.
	out, err := runCmd(t, pipelineCmd(), &fakeEndorser{})
	require.NoError(t, err)
	require.Contains(t, out, "relayer      : ok")
	require.Contains(t, out, "channel mychannel")
	require.Contains(t, out, `mst_outbox_entries{status="DONE"} 9`)
	require.Contains(t, out, "mst_relayer_balance_gwei 500000")
}

func TestPipelineMetricsNotEnabled(t *testing.T) {
	t.Cleanup(func() {
		viper.Reset()
		resetFlags()
	})
	viper.Reset() // mst.metricsAddr unset
	_, err := runCmd(t, pipelineCmd(), &fakeEndorser{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "metricsAddr")
}
