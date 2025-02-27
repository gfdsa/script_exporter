package exporter

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// runScript runs a program with some arguments; the program is
// args[0]. The timeout argument is in seconds, and if it's larger
// than zero, it is exported into the environment as $SCRIPT_TIMEOUT
// (its raw value) and $SCRIPT_DEADLINE, which is the Unix timestamp
// (including fractional parts) when the deadline will expire. If
// enforced is true, the timeout will be enforced by script_exporter,
// by killing the script if the timeout is reached, and
// $SCRIPT_TIMEOUT_ENFORCED will be set to 1 in the environment to
// inform the script of this.
//
// Note that killing the script is only a best effort attempt to
// terminate its execution and time out the request. Sub-processes may
// not be terminated, and termination may not be entirely successful.
//
// Tentatively, we do not inherit the context from the HTTP request.
// Doing so would provide automatic termination should the client
// close the connection, but it would mean that all scripts would
// be subject to abrupt termination regardless of any 'enforced:'
// settings. Right now, abrupt termination requires opting in in
// the configuration file.
func runScript(timeout float64, enforced bool, args []string) (string, int, error) {
	var output []byte
	var err error

	// We go through a great deal of work to get a deadline with
	// fractional seconds that we can expose in an environment
	// variable. However, this is pretty much necessary since
	// we've copied Blackbox's default of a half second adjustment
	// to the raw Prometheus timeout.  We can hardly do that and
	// then round our deadlines (or our raw timeouts) off to full
	// seconds.
	ns := float64(time.Second)
	deadline := time.Now().Add(time.Duration(timeout * ns))
	dlfractional := float64(deadline.UnixNano()) / ns

	var cmd *exec.Cmd
	var cancel context.CancelFunc
	ctx := context.Background()
	if timeout > 0 && enforced {
		ctx, cancel = context.WithDeadline(context.Background(), deadline)
		defer cancel()
	}
	cmd = exec.CommandContext(ctx, args[0], args[1:]...)

	if timeout > 0 {
		cmd.Env = os.Environ()
		// Three digits of fractional precision in the seconds and
		// the deadline are probably excessive, given that we're
		// running external programs. But better slightly excessive
		// than not enough precision.
		cmd.Env = append(cmd.Env, fmt.Sprintf("SCRIPT_TIMEOUT=%0.3f", timeout))
		cmd.Env = append(cmd.Env, fmt.Sprintf("SCRIPT_DEADLINE=%0.3f", dlfractional))
		var ienforced int
		if enforced {
			ienforced = 1
		}
		cmd.Env = append(cmd.Env, fmt.Sprintf("SCRIPT_TIMEOUT_ENFORCED=%d", ienforced))
	}

	output, err = cmd.Output()
	if err != nil {
		if exitError, ok := err.(*exec.ExitError); ok {
			return "", exitError.ExitCode(), err
		}

		return "", -1, err
	}

	return string(output), 0, nil
}

// getTimeout gets the Prometheus scrape timeout (in seconds) from the
// HTTP request, either from a 'timeout' query parameter or from the
// special HTTP header that Prometheus inserts on scrapes, and returns
// it or 0 on error.  If there is a timeout, it is modified down by
// the offset.
func getTimeout(r *http.Request, offset float64, maxTimeout float64) float64 {
	v := r.URL.Query().Get("timeout")
	if v == "" {
		v = r.Header.Get("X-Prometheus-Scrape-Timeout-Seconds")
	}
	if v == "" {
		return 0
	}
	ts, err := strconv.ParseFloat(v, 64)
	adjusted := ts - offset
	switch {
	case err != nil:
		return 0
	case maxTimeout < adjusted && maxTimeout > 0:
		return maxTimeout
	case adjusted <= 0:
		return 0
	default:
		return adjusted
	}
}

// instrumentScript wraps the underlying http.Handler with Prometheus
// instrumentation to produce per-script metrics on the number of
// requests in flight, the number of requests in total, and the
// distribution of their duration. Requests without a 'script=' query
// parameter are not instrumented (and will probably be rejected).
func instrumentScript(obs prometheus.ObserverVec, cnt *prometheus.CounterVec, g *prometheus.GaugeVec, next http.Handler) http.HandlerFunc {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sn := r.URL.Query().Get("script")
		if sn == "" {
			// Rather than make up a fake script label, such
			// as "NONE", we let the request fall through without
			// instrumenting it. Under normal circumstances it
			// will fail anyway, as metricsHandler() will
			// reject it.
			next.ServeHTTP(w, r)
			return
		}

		labels := prometheus.Labels{"script": sn}
		g.With(labels).Inc()
		defer g.With(labels).Dec()
		now := time.Now()
		next.ServeHTTP(w, r)
		obs.With(labels).Observe(time.Since(now).Seconds())
		cnt.With(labels).Inc()
	})
}
