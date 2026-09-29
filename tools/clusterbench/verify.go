package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/gorilla/websocket"
)

// check is one acceptance line: what was expected, what was observed.
type check struct {
	Name     string
	Target   string
	Measured string
	Pass     bool
}

func printChecks(out io.Writer, checks []check) {
	writer := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(writer, "CHECK\tTARGET\tMEASURED\tRESULT")
	for _, item := range checks {
		result := "PASS"
		if !item.Pass {
			result = "FAIL"
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\n", item.Name, item.Target, item.Measured, result)
	}
	_ = writer.Flush()
}

func allPass(checks []check) bool {
	for _, item := range checks {
		if !item.Pass {
			return false
		}
	}
	return true
}

func countStatuses(statuses []int) (ok, limited, other int) {
	for _, status := range statuses {
		switch status {
		case http.StatusOK:
			ok++
		case http.StatusTooManyRequests:
			limited++
		default:
			other++
		}
	}
	return ok, limited, other
}

// judgeRPM expects exactly the limit to succeed and every other request to be
// rate limited; any other status (such as a 503 from a Redis timeout) fails.
func judgeRPM(statuses []int, limit int) check {
	ok, limited, other := countStatuses(statuses)
	return check{
		Name:     "rpm",
		Target:   fmt.Sprintf("%d×200 / %d×429 of %d", limit, len(statuses)-limit, len(statuses)),
		Measured: fmt.Sprintf("%d×200 / %d×429 / %d other", ok, limited, other),
		Pass:     ok == limit && limited == len(statuses)-limit && other == 0,
	}
}

// judgeQuota expects sequential requests to be admitted while the accumulated
// cost is below the limit and the first request after it to be rejected.
func judgeQuota(statuses []int, wantAdmitted int, usedUSD, limitUSD float64) check {
	admitted := 0
	for _, status := range statuses {
		if status != http.StatusOK {
			break
		}
		admitted++
	}
	rejectedNext := admitted < len(statuses) && statuses[admitted] == http.StatusTooManyRequests
	return check{
		Name:     "cost-limit",
		Target:   fmt.Sprintf("%d admitted, next 429, used ≥ %.4f USD", wantAdmitted, limitUSD),
		Measured: fmt.Sprintf("%d admitted, next %s, used %.4f USD", admitted, nextStatus(statuses, admitted), usedUSD),
		Pass:     admitted == wantAdmitted && rejectedNext && usedUSD >= limitUSD,
	}
}

func nextStatus(statuses []int, index int) string {
	if index >= len(statuses) {
		return "none"
	}
	return fmt.Sprint(statuses[index])
}

func judgePropagation(latencies []time.Duration, budget time.Duration) check {
	worst := time.Duration(0)
	for _, latency := range latencies {
		worst = max(worst, latency)
	}
	return check{
		Name:     "config-propagation",
		Target:   "≤ " + budget.String(),
		Measured: "max " + worst.Round(time.Millisecond).String(),
		Pass:     len(latencies) > 0 && worst <= budget,
	}
}

type verifyConfig struct {
	instances   []string
	lb          string
	authKey     string
	upstreamURL string
}

func runVerify(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	instances := flags.String("instances", "http://gpt-load-1:3001,http://gpt-load-2:3001,http://gpt-load-3:3001", "cluster instance base URLs")
	lb := flags.String("lb", "http://nginx", "load balancer base URL")
	upstream := flags.String("upstream", "http://fake-upstream:8080", "fake upstream base URL for the openai channel (the channel appends /v1)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg := verifyConfig{instances: splitList(*instances), lb: strings.TrimRight(*lb, "/"), authKey: os.Getenv("AUTH_KEY"), upstreamURL: *upstream}
	if len(cfg.instances) < 3 || cfg.authKey == "" {
		return errors.New("verify needs at least 3 -instances and AUTH_KEY")
	}
	checks, err := verifyCluster(ctx, cfg)
	printChecks(os.Stdout, checks)
	if err != nil {
		return err
	}
	if !allPass(checks) {
		return errors.New("cluster acceptance failed")
	}
	return nil
}

func verifyCluster(ctx context.Context, cfg verifyConfig) ([]check, error) {
	admin := adminClient{baseURL: cfg.instances[0], authKey: cfg.authKey, http: &http.Client{Timeout: 30 * time.Second}}
	suffix := fmt.Sprintf("verify-%d", time.Now().UnixNano())
	set, err := seed(ctx, admin, append([]string{cfg.lb}, cfg.instances...), cfg.upstreamURL, suffix)
	if err != nil {
		return nil, fmt.Errorf("seed: %w", err)
	}
	client := newDataPlaneClient()
	warmUp(ctx, client, cfg.instances, set.bench.Secret, 70)

	var checks []check
	checks = append(checks, judgeRPM(burst(ctx, client, cfg.instances, set.rpm.Secret, 200), rpmLimit))

	quotaStatuses, err := quotaSequential(ctx, admin, client, cfg.instances, set.quota, 20)
	if err != nil {
		return checks, err
	}
	used, err := admin.costUsedUSD(ctx, set.quota.ID)
	if err != nil {
		return checks, err
	}
	checks = append(checks, judgeQuota(quotaStatuses, int(math.Ceil(quotaRequests)), used, quotaRequests*requestCostUSD))

	latencies, err := propagation(ctx, admin, client, cfg.instances, set)
	if err != nil {
		return checks, err
	}
	checks = append(checks, judgePropagation(latencies, time.Second))

	checks = append(checks, websocketThroughLB(ctx, cfg.lb, set.probe.Secret))
	return checks, nil
}

// warmUp opens enough gateway → Redis connections on every instance that the
// RPM burst measures the scripts rather than connection dialing.
func warmUp(ctx context.Context, client *http.Client, instances []string, secret string, perInstance int) {
	var wg sync.WaitGroup
	for _, instance := range instances {
		for range perInstance {
			wg.Go(func() { _, _ = chat(ctx, client, instance, secret, benchModel) })
		}
	}
	wg.Wait()
}

// burst fires total requests at once, spread round-robin over instances.
func burst(ctx context.Context, client *http.Client, instances []string, secret string, total int) []int {
	statuses := make([]int, total)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range total {
		wg.Go(func() {
			<-start
			status, err := chat(ctx, client, instances[i%len(instances)], secret, benchModel)
			if err != nil {
				status = -1
			}
			statuses[i] = status
		})
	}
	close(start)
	wg.Wait()
	return statuses
}

// quotaSequential sends requests one at a time round-robin until the first
// non-200 response or maxRequests. The gateway settles a request's cost after
// flushing its response, so before the next request it waits until the shared
// usage includes every admitted request; the check then measures whether the
// instances share one exact count rather than racing that settlement window.
func quotaSequential(ctx context.Context, admin adminClient, client *http.Client, instances []string, key accessKey, maxRequests int) ([]int, error) {
	var statuses []int
	for i := range maxRequests {
		status, err := chat(ctx, client, instances[i%len(instances)], key.Secret, benchModel)
		if err != nil {
			status = -1
		}
		statuses = append(statuses, status)
		if status != http.StatusOK {
			return statuses, nil
		}
		if err := awaitSettledCost(ctx, admin, key.ID, float64(len(statuses))*requestCostUSD); err != nil {
			return statuses, err
		}
	}
	return statuses, nil
}

func awaitSettledCost(ctx context.Context, admin adminClient, id uint, wantUSD float64) error {
	deadline := time.Now().Add(2 * time.Second)
	for {
		used, err := admin.costUsedUSD(ctx, id)
		if err != nil {
			return err
		}
		if used+1e-9 >= wantUSD {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("access key %d cost stayed at %.4f USD, want %.4f", id, used, wantUSD)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// propagation adds a model on the first instance and measures how long the
// other instances take to route it.
func propagation(ctx context.Context, admin adminClient, client *http.Client, instances []string, set fixtureSet) ([]time.Duration, error) {
	for _, instance := range instances[1:] {
		if status, err := chat(ctx, client, instance, set.probe.Secret, set.propagationModel); err == nil && status == http.StatusOK {
			return nil, fmt.Errorf("%s already serves %s before the change", instance, set.propagationModel)
		}
	}
	if err := admin.setGroupModels(ctx, set.groupID, []string{benchModel, set.propagationModel}); err != nil {
		return nil, err
	}
	changed := time.Now()
	latencies := make([]time.Duration, len(instances)-1)
	errs := make([]error, len(instances)-1)
	var wg sync.WaitGroup
	for i, instance := range instances[1:] {
		wg.Go(func() {
			deadline := changed.Add(10 * time.Second)
			for time.Now().Before(deadline) {
				if status, err := chat(ctx, client, instance, set.probe.Secret, set.propagationModel); err == nil && status == http.StatusOK {
					latencies[i] = time.Since(changed)
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
			errs[i] = fmt.Errorf("%s never served %s", instance, set.propagationModel)
		})
	}
	wg.Wait()
	return latencies, errors.Join(errs...)
}

// websocketThroughLB proves the load balancer forwards the upgrade and the
// Host header: the browser-style Origin only passes the gateway's same-origin
// check when Host reaches it unchanged.
func websocketThroughLB(ctx context.Context, lb, secret string) check {
	result := check{Name: "websocket-via-lb", Target: "101 Switching Protocols"}
	header := http.Header{"Authorization": {"Bearer " + secret}, "Origin": {lb}}
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, response, err := websocket.DefaultDialer.DialContext(dialCtx, "ws"+strings.TrimPrefix(lb, "http")+"/v1/responses", header)
	switch {
	case err == nil:
		result.Measured = fmt.Sprint(response.StatusCode)
		result.Pass = response.StatusCode == http.StatusSwitchingProtocols
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
		_ = conn.Close()
	case response != nil:
		body, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		result.Measured = fmt.Sprintf("%d %s", response.StatusCode, strings.TrimSpace(string(body)))
	default:
		result.Measured = err.Error()
	}
	return result
}
