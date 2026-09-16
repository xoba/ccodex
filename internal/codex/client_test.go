package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/xoba/ccodex/internal/buildinfo"
)

// The test binary doubles as an app-server. Tests exercise real pipes,
// subprocess shutdown, and cancellation without reading a real Codex account.
func TestMain(m *testing.M) {
	if os.Getenv("CCODEX_TEST_HELPER") == "1" {
		os.Exit(runHelper())
	}
	os.Exit(m.Run())
}

type helperRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Error  *rpcError       `json:"error"`
}

func setupHelper(t *testing.T, scenario string) (Options, string, string) {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	logPath, pidPath := filepath.Join(dir, "requests.jsonl"), filepath.Join(dir, "pid")
	t.Setenv("CCODEX_TEST_HELPER", "1")
	t.Setenv("CCODEX_TEST_SCENARIO", scenario)
	t.Setenv("CCODEX_TEST_LOG", logPath)
	t.Setenv("CCODEX_TEST_PID", pidPath)
	return Options{Binary: binary, Timeout: 5 * time.Second}, logPath, pidPath
}

func readRequests(t *testing.T, path string) []helperRequest {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var requests []helperRequest
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var request helperRequest
		if err := json.Unmarshal([]byte(line), &request); err != nil {
			t.Fatal(err)
		}
		requests = append(requests, request)
	}
	return requests
}

func assertHelperStopped(t *testing.T, pidPath string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return // Signal 0 is not supported on Windows.
	}
	data, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		t.Fatal(err)
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	defer process.Release()
	if err := process.Signal(syscall.Signal(0)); err == nil {
		process.Kill()
		t.Fatalf("app-server process %d was left running", pid)
	}
}

func TestFetchReadsOnlyAccountEndpointsAndHandlesServerMessages(t *testing.T) {
	opts, logPath, pidPath := setupHelper(t, "success")
	before := time.Now()
	snapshot, err := Fetch(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	assertHelperStopped(t, pidPath)
	if snapshot.Account.Type != "chatgpt" || snapshot.Account.Email == nil || *snapshot.Account.Email != "test@example.com" {
		t.Fatalf("unexpected account: %+v", snapshot.Account)
	}
	if snapshot.FetchedAt.Before(before) || snapshot.FetchedAt.After(time.Now()) || snapshot.FetchedAt.Location() != time.UTC {
		t.Fatalf("unexpected timestamp: %v", snapshot.FetchedAt)
	}
	if snapshot.RateLimits == nil || snapshot.RateLimits.RateLimits.Primary == nil {
		t.Fatalf("missing rate limits: %+v", snapshot.RateLimits)
	}
	if snapshot.RateLimits.AccountID == nil || *snapshot.RateLimits.AccountID != "account-123" {
		t.Fatalf("missing account identity: %+v", snapshot.RateLimits.AccountID)
	}
	window := snapshot.RateLimits.RateLimits.Primary
	if window.UsedPercent == nil || *window.UsedPercent != 37.5 || window.WindowDurationMins == nil || *window.WindowDurationMins != 300 {
		t.Fatalf("unexpected primary window: %+v", window)
	}
	if got := snapshot.RateLimits.RateLimitsByLimitID["codex"].Secondary; got == nil || got.UsedPercent != nil || got.ResetsAt != nil {
		t.Fatalf("missing data must stay unknown: %+v", got)
	}
	if snapshot.RateLimits.OrdinaryUsageAllowed == nil || *snapshot.RateLimits.OrdinaryUsageAllowed {
		t.Fatal("explicit false ordinaryUsageAllowed was lost")
	}
	if snapshot.Usage == nil || snapshot.Usage.Summary.LifetimeTokens == nil || *snapshot.Usage.Summary.LifetimeTokens != 1234567 {
		t.Fatalf("unexpected usage: %+v", snapshot.Usage)
	}
	if snapshot.Usage.Summary.PeakDailyTokens != nil || len(snapshot.Usage.DailyUsageBuckets) != 1 || len(snapshot.Warnings) != 0 {
		t.Fatalf("unexpected optional values: %+v", snapshot)
	}
	requests := readRequests(t, logPath)
	var methods []string
	for _, request := range requests {
		if request.Method != "" {
			methods = append(methods, request.Method)
		}
	}
	want := []string{"initialize", "initialized", "account/read", "account/rateLimits/read", "account/usage/read"}
	if !reflect.DeepEqual(methods, want) {
		t.Fatalf("methods = %v, want %v", methods, want)
	}
	var init struct {
		ClientInfo struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"clientInfo"`
	}
	if err := json.Unmarshal(requests[0].Params, &init); err != nil || init.ClientInfo.Name != "ccodex" || init.ClientInfo.Version != buildinfo.Version {
		t.Fatalf("unexpected initialization: %s", requests[0].Params)
	}
	if string(requests[2].Params) != `{"refreshToken":false}` {
		t.Fatalf("account read must not request token refresh: %s", requests[2].Params)
	}
	if requests[4].Error == nil || requests[4].Error.Code != -32601 || string(requests[4].ID) != `"server-request"` {
		t.Fatalf("server request was not rejected: %+v", requests[4])
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"rateLimitsByLimitId"`) || !strings.Contains(string(data), `"usedPercent":null`) {
		t.Fatalf("JSON shape or unknown values were lost: %s", data)
	}
}

func TestFetchOptionalUsageFailuresPreserveRateLimits(t *testing.T) {
	for _, tc := range []struct{ scenario, warning string }{
		{"usage-unsupported", "does not support token activity"},
		{"usage-error", "Token activity is unavailable"},
		{"usage-invalid", "Token activity is unavailable"},
		{"usage-hang", "Token activity timed out"},
	} {
		t.Run(tc.scenario, func(t *testing.T) {
			opts, _, pidPath := setupHelper(t, tc.scenario)
			if tc.scenario == "usage-hang" {
				opts.Timeout = time.Second
			}
			before := time.Now()
			snapshot, err := Fetch(context.Background(), opts)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.RateLimits == nil || snapshot.Usage != nil || len(snapshot.Warnings) != 1 || !strings.Contains(snapshot.Warnings[0], tc.warning) {
				t.Fatalf("unexpected partial snapshot: %+v", snapshot)
			}
			if strings.Contains(snapshot.Warnings[0], "private-marker") {
				t.Fatal("raw server error leaked into warning")
			}
			if time.Since(before) > opts.Timeout+time.Second {
				t.Fatal("optional request did not respect timeout")
			}
			assertHelperStopped(t, pidPath)
		})
	}
}

func TestFetchRejectsUnavailableAuthentication(t *testing.T) {
	for _, tc := range []struct{ scenario, hint string }{
		{"logged-out", "not signed in"},
		{"api-key", "require a ChatGPT login"},
	} {
		t.Run(tc.scenario, func(t *testing.T) {
			opts, logPath, pidPath := setupHelper(t, tc.scenario)
			_, err := Fetch(context.Background(), opts)
			if err == nil || !strings.Contains(err.Error(), tc.hint) || !strings.Contains(err.Error(), "codex login") {
				t.Fatalf("error = %v, want helpful login error", err)
			}
			if got := len(readRequests(t, logPath)); got != 3 {
				t.Fatalf("sent requests after unavailable authentication: %d", got)
			}
			assertHelperStopped(t, pidPath)
		})
	}
}

func TestFetchRejectsProtocolFailuresWithoutLeakingRemoteText(t *testing.T) {
	for _, tc := range []struct{ scenario, hint string }{
		{"invalid-json", "malformed JSON"},
		{"oversized", "exceeds 4 MiB"},
		{"wrong-id", "unexpected request ID"},
		{"both-result-error", "invalid response"},
		{"null-result", "invalid result"},
		{"missing-limits", "no rate-limit snapshot"},
		{"init-unsupported", "update Codex CLI"},
		{"limits-error", "RPC code -32000"},
		{"early-exit", "closed its output"},
	} {
		t.Run(tc.scenario, func(t *testing.T) {
			opts, _, pidPath := setupHelper(t, tc.scenario)
			snapshot, err := Fetch(context.Background(), opts)
			if err == nil || !strings.Contains(err.Error(), tc.hint) {
				t.Fatalf("snapshot = %+v, error = %v; want %q", snapshot, err, tc.hint)
			}
			if strings.Contains(err.Error(), "private-marker") {
				t.Fatalf("remote text leaked: %v", err)
			}
			assertHelperStopped(t, pidPath)
		})
	}
}

func TestFetchOverallDeadlineKillsAndReapsAppServer(t *testing.T) {
	opts, _, pidPath := setupHelper(t, "init-hang")
	opts.Timeout = 300 * time.Millisecond
	before := time.Now()
	_, err := Fetch(context.Background(), opts)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want deadline exceeded", err)
	}
	if time.Since(before) > 2*time.Second {
		t.Fatal("hung server was not stopped promptly")
	}
	assertHelperStopped(t, pidPath)
}

func TestFetchCancellationKillsAndReapsAppServer(t *testing.T) {
	opts, _, pidPath := setupHelper(t, "init-hang")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := Fetch(ctx, opts)
		result <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(pidPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not stop the request")
	}
	assertHelperStopped(t, pidPath)
}

func TestFetchMissingBinaryAndInvalidTimeout(t *testing.T) {
	_, err := Fetch(context.Background(), Options{Binary: filepath.Join(t.TempDir(), "missing-codex")})
	if err == nil || !strings.Contains(err.Error(), "install Codex CLI") {
		t.Fatalf("missing binary error = %v", err)
	}
	_, err = Fetch(context.Background(), Options{Timeout: -time.Second})
	if err == nil || !strings.Contains(err.Error(), "timeout must be positive") {
		t.Fatalf("negative timeout error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = Fetch(ctx, Options{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled context error = %v", err)
	}
}

func runHelper() int {
	if !reflect.DeepEqual(os.Args[1:], []string{"app-server", "--listen", "stdio://"}) {
		return 11
	}
	if err := os.WriteFile(os.Getenv("CCODEX_TEST_PID"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		return 12
	}
	log, err := os.OpenFile(os.Getenv("CCODEX_TEST_LOG"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return 13
	}
	defer log.Close()
	fmt.Fprintln(os.Stderr, "private-marker: raw diagnostic log")
	scenario := os.Getenv("CCODEX_TEST_SCENARIO")
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	read := func() (helperRequest, bool) {
		var req helperRequest
		if !scanner.Scan() {
			return req, false
		}
		if _, err := fmt.Fprintln(log, scanner.Text()); err != nil {
			return req, false
		}
		return req, json.Unmarshal(scanner.Bytes(), &req) == nil
	}
	respond := func(req helperRequest, result any) {
		encoder.Encode(map[string]any{"id": req.ID, "result": result})
	}
	reject := func(req helperRequest, code int) {
		encoder.Encode(map[string]any{"id": req.ID, "error": map[string]any{
			"code": code, "message": "private-marker: upstream error", "data": "private-marker",
		}})
	}
	for {
		req, ok := read()
		if !ok {
			return 0
		}
		switch req.Method {
		case "initialize":
			switch scenario {
			case "init-hang":
				time.Sleep(time.Minute)
				return 0
			case "invalid-json":
				fmt.Fprintln(os.Stdout, "private-marker invalid JSON")
				continue
			case "oversized":
				fmt.Fprintln(os.Stdout, strings.Repeat("x", maxMessageSize+1))
				continue
			case "wrong-id":
				encoder.Encode(map[string]any{"id": 999, "result": map[string]any{}})
				continue
			case "both-result-error":
				encoder.Encode(map[string]any{"id": req.ID, "result": map[string]any{}, "error": map[string]any{"code": 9}})
				continue
			case "null-result":
				respond(req, nil)
				continue
			case "init-unsupported":
				reject(req, -32601)
				continue
			case "early-exit":
				return 1
			}
			respond(req, map[string]any{"userAgent": "test"})
		case "initialized":
			// No reply to notifications.
		case "account/read":
			var account any = map[string]any{"type": "chatgpt", "email": "test@example.com", "planType": "pro"}
			if scenario == "logged-out" {
				account = nil
			} else if scenario == "api-key" {
				account = map[string]any{"type": "apiKey"}
			} else if scenario == "account-no-email" {
				account = map[string]any{"type": "chatgpt", "email": nil, "planType": "pro"}
			}
			respond(req, map[string]any{"account": account, "requiresOpenaiAuth": true})
		case "account/rateLimits/read":
			if scenario == "missing-limits" || scenario == "reset-refresh-missing" {
				respond(req, map[string]any{})
				continue
			}
			if scenario == "limits-error" || scenario == "reset-refresh-error" {
				reject(req, -32000)
				continue
			}
			if scenario == "reset-refresh-hang" {
				time.Sleep(time.Minute)
				return 0
			}
			if scenario == "reset-refresh-exit" {
				return 0
			}
			if scenario == "success" {
				fmt.Fprintln(os.Stdout)
				encoder.Encode(map[string]any{"method": "account/rateLimits/updated", "params": map[string]any{}})
				encoder.Encode(map[string]any{"id": "server-request", "method": "account/chatgptAuthTokens/refresh", "params": map[string]any{}})
				response, ok := read()
				if !ok || response.Error == nil || response.Error.Code != -32601 {
					return 14
				}
			}
			var limits any
			json.Unmarshal([]byte(`{"rateLimits":{"limitId":"codex","primary":{"usedPercent":37.5,"windowDurationMins":300,"resetsAt":1790000000},"credits":{"hasCredits":true,"unlimited":false,"balance":"2.50"}},"rateLimitsByLimitId":{"codex":{"secondary":{"usedPercent":null,"windowDurationMins":10080,"resetsAt":null}}},"ordinaryUsageAllowed":false,"rateLimitResetCredits":{"availableCount":2,"credits":[{"id":"credit-123","resetType":"codexRateLimits","status":"available","grantedAt":1789000000,"expiresAt":1791000000,"title":"Earned reset","description":null}]}}`), &limits)
			if scenario != "reset-accountid-missing" {
				limits.(map[string]any)["accountId"] = "account-123"
			}
			if scenario == "reset-accountid-null" {
				limits.(map[string]any)["accountId"] = nil
			} else if scenario == "reset-accountid-blank" {
				limits.(map[string]any)["accountId"] = " "
			}
			respond(req, limits)
		case "account/rateLimitResetCredit/consume":
			switch scenario {
			case "reset-consume-error":
				reject(req, -32000)
			case "reset-consume-exit":
				return 0
			case "reset-consume-hang":
				time.Sleep(time.Minute)
				return 0
			case "reset-consume-unknown":
				respond(req, map[string]any{"outcome": "private-marker"})
			case "reset-consume-missing":
				respond(req, map[string]any{})
			case "reset-consume-malformed":
				respond(req, map[string]any{"outcome": 123})
			case "reset-outcome-alreadyRedeemed":
				respond(req, map[string]any{"outcome": "alreadyRedeemed"})
			case "reset-outcome-nothingToReset":
				respond(req, map[string]any{"outcome": "nothingToReset"})
			case "reset-outcome-noCredit":
				respond(req, map[string]any{"outcome": "noCredit"})
			default:
				respond(req, map[string]any{"outcome": "reset"})
			}
		case "account/usage/read":
			switch scenario {
			case "usage-unsupported":
				reject(req, -32601)
			case "usage-error":
				reject(req, -32000)
			case "usage-invalid":
				respond(req, "private-marker")
			case "usage-hang":
				time.Sleep(time.Minute)
				return 0
			default:
				respond(req, map[string]any{
					"summary":           map[string]any{"lifetimeTokens": 1234567, "peakDailyTokens": nil},
					"dailyUsageBuckets": []map[string]any{{"startDate": "2026-09-14", "tokens": 1234}},
				})
			}
		default:
			return 15
		}
	}
}
