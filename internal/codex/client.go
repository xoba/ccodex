package codex

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/xoba/ccodex/internal/buildinfo"
)

const (
	defaultTimeout = 15 * time.Second
	usageTimeout   = 3 * time.Second
	shutdownGrace  = 150 * time.Millisecond
	maxMessageSize = 4 << 20
)

// ErrAccountChanged means a reset's expected identity could not be confirmed.
// No reset request was sent. Automatic callers should pause instead of carrying
// an existing reset attempt over to another account.
var ErrAccountChanged = errors.New("Codex account identity changed or is unavailable")

type Options struct {
	// Binary defaults to codex, resolved through PATH.
	Binary string
	// Timeout covers startup and all requests. Zero uses 15 seconds.
	Timeout time.Duration
}

// Fetch starts a temporary app-server and reads the existing ChatGPT login,
// rate limits, and optional token activity. It never starts a Codex turn.
func Fetch(ctx context.Context, opts Options) (*Snapshot, error) {
	ctx, cancel, err := operationContext(ctx, opts.Timeout)
	if err != nil {
		return nil, err
	}
	defer cancel()
	client, account, err := openAccount(ctx, opts.Binary)
	if err != nil {
		return nil, err
	}
	defer client.close()
	limits, err := client.readRateLimits(ctx)
	if err != nil {
		return nil, err
	}
	snapshot := &Snapshot{
		FetchedAt:  time.Now().UTC(),
		Account:    account,
		RateLimits: limits,
	}

	// Token history is optional. Reserve time within the overall deadline so a
	// slow or unsupported history endpoint cannot discard a usable quota read.
	optionalTimeout := usageTimeout
	if deadline, ok := ctx.Deadline(); ok {
		optionalTimeout = min(optionalTimeout, time.Until(deadline)/2)
	}
	usageCtx, usageCancel := context.WithTimeout(ctx, optionalTimeout)
	defer usageCancel()
	var usage UsageResponse
	err = client.call(usageCtx, "account/usage/read", nil, &usage)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		warning := "Token activity is unavailable; rate limits were fetched successfully."
		var rpcErr *rpcError
		if errors.Is(err, context.DeadlineExceeded) {
			warning = "Token activity timed out; rate limits were fetched successfully."
		} else if errors.As(err, &rpcErr) && rpcErr.Code == -32601 {
			warning = "This Codex version does not support token activity; update Codex CLI to enable it."
		}
		snapshot.Warnings = []string{warning}
	} else {
		snapshot.Usage = &usage
	}
	return snapshot, nil
}

// Reset makes exactly one reset-credit redemption request. It does not check
// availability first: the same idempotency key can confirm a previous success
// even after the last credit was consumed. It never retries automatically.
func Reset(ctx context.Context, opts Options, params ResetParams) (*ResetResult, error) {
	if strings.TrimSpace(params.IdempotencyKey) == "" {
		return nil, errors.New("reset requires a nonempty idempotency key; reuse the same key when retrying an attempt")
	}
	if params.CreditID != "" && strings.TrimSpace(params.CreditID) == "" {
		return nil, errors.New("reset credit ID must not contain only whitespace")
	}
	ctx, cancel, err := operationContext(ctx, opts.Timeout)
	if err != nil {
		return nil, err
	}
	defer cancel()
	client, account, err := openAccount(ctx, opts.Binary)
	if err != nil {
		var identityErr *accountIdentityError
		if (params.ExpectedAccount != nil || params.ExpectedAccountID != "") && errors.As(err, &identityErr) {
			return nil, fmt.Errorf("%w; reset was not attempted: %w", ErrAccountChanged, err)
		}
		return nil, err
	}
	defer client.close()
	if expected := params.ExpectedAccount; expected != nil {
		if account.Type != expected.Type || !sameOptionalString(account.Email, expected.Email) {
			return nil, fmt.Errorf("%w; the signed-in account does not match the expected account; reset was not attempted", ErrAccountChanged)
		}
	}
	if params.ExpectedAccountID != "" {
		limits, err := client.readRateLimits(ctx)
		if err != nil {
			return nil, fmt.Errorf("%w; could not verify the expected account ID; reset was not attempted: %w", ErrAccountChanged, err)
		}
		if limits.AccountID == nil || strings.TrimSpace(*limits.AccountID) == "" || *limits.AccountID != params.ExpectedAccountID {
			return nil, fmt.Errorf("%w; the current account ID is missing or does not match the expected account; reset was not attempted", ErrAccountChanged)
		}
	}
	var response struct {
		Outcome string `json:"outcome"`
	}
	if err := client.call(ctx, "account/rateLimitResetCredit/consume", params, &response); err != nil {
		return nil, fmt.Errorf("reset outcome may be unknown; retry only with the same idempotency key: %w", err)
	}
	switch response.Outcome {
	case "reset", "alreadyRedeemed", "nothingToReset", "noCredit":
	default:
		return nil, errors.New("Codex returned an unrecognized reset outcome; reset outcome may be unknown; retry only with the same idempotency key")
	}
	result := &ResetResult{
		Outcome:        response.Outcome,
		IdempotencyKey: params.IdempotencyKey,
		Account:        account,
		FetchedAt:      time.Now().UTC(),
	}
	limits, err := client.readRateLimits(ctx)
	if err != nil {
		// The mutation has a confirmed outcome. Keep that evidence even when
		// the read fails or the caller cancels during the follow-up request.
		result.Warnings = []string{"The reset outcome is confirmed, but updated rate limits are unavailable; run `ccodex status` to refresh them."}
		return result, nil
	}
	result.RateLimits = limits
	result.FetchedAt = time.Now().UTC()
	return result, nil
}

func operationContext(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc, error) {
	if timeout < 0 {
		return nil, nil, errors.New("Codex timeout must be positive")
	}
	if timeout == 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	return ctx, cancel, nil
}

func openAccount(ctx context.Context, binary string) (*client, *Account, error) {
	if binary == "" {
		binary = "codex"
	}
	c, err := start(ctx, binary)
	if err != nil {
		return nil, nil, err
	}
	account, err := c.initializeAccount(ctx)
	if err != nil {
		c.close()
		return nil, nil, err
	}
	return c, account, nil
}

func (c *client) initializeAccount(ctx context.Context) (*Account, error) {
	var initialized struct{}
	if err := c.call(ctx, "initialize", map[string]any{
		"clientInfo": map[string]string{"name": "ccodex", "version": buildinfo.Version},
	}, &initialized); err != nil {
		return nil, fmt.Errorf("initialize Codex app-server: %w", err)
	}
	if err := c.write(ctx, map[string]string{"method": "initialized"}); err != nil {
		return nil, fmt.Errorf("initialize Codex app-server: %w", err)
	}
	var response struct {
		Account *Account `json:"account"`
	}
	if err := c.call(ctx, "account/read", map[string]bool{"refreshToken": false}, &response); err != nil {
		return nil, &accountIdentityError{fmt.Errorf("read Codex account: %w", err)}
	}
	if response.Account == nil {
		return nil, &accountIdentityError{errors.New("Codex is not signed in; run `codex login` and sign in with ChatGPT")}
	}
	if response.Account.Type != "chatgpt" {
		return nil, &accountIdentityError{errors.New("Codex subscription limits require a ChatGPT login; check `codex login status` and sign in with ChatGPT using `codex login`")}
	}
	return response.Account, nil
}

// Preserve the ordinary account-read error text while allowing a bound reset
// to distinguish unavailable identity from startup and protocol initialization.
type accountIdentityError struct{ error }

func (e *accountIdentityError) Unwrap() error { return e.error }

func sameOptionalString(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func (c *client) readRateLimits(ctx context.Context) (*RateLimitsResponse, error) {
	var limits RateLimitsResponse
	if err := c.call(ctx, "account/rateLimits/read", nil, &limits); err != nil {
		return nil, fmt.Errorf("read Codex rate limits: %w", err)
	}
	if limits.RateLimits == nil && len(limits.RateLimitsByLimitID) == 0 {
		return nil, errors.New("Codex returned no rate-limit snapshot; update Codex CLI and try again")
	}
	return &limits, nil
}

// wireMessage keeps remote error text out of user-visible errors: it can
// contain upstream response bodies, credentials, or terminal escape sequences.
type wireMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

type rpcError struct {
	Code int64 `json:"code"`
}

func (e *rpcError) Error() string {
	if e.Code == -32601 {
		return "method is unavailable; update Codex CLI and try again"
	}
	return fmt.Sprintf("Codex request failed (RPC code %d); check `codex login status` and try again", e.Code)
}

type received struct {
	message wireMessage
	err     error
}

type client struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   *os.File
	messages chan received
	stop     chan struct{}
	readDone chan struct{}
	waitDone chan error
	nextID   int64
}

func start(ctx context.Context, binary string) (*client, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, binary, "app-server", "--listen", "stdio://")
	// Own the stdout pipe: cmd.Wait must not close it before the reader drains
	// a final response when the subprocess exits immediately afterward.
	stdout, output, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create Codex response pipe: %w", err)
	}
	cmd.Stdout = output
	// A nil stderr sends it to the null device without retaining sensitive logs.
	cmd.Stderr = nil
	stdin, err := cmd.StdinPipe()
	if err != nil {
		stdout.Close()
		output.Close()
		return nil, fmt.Errorf("create Codex request pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		output.Close()
		return nil, fmt.Errorf("start Codex app-server (install Codex CLI or set --codex-bin): %w", err)
	}
	output.Close()
	c := &client{
		cmd: cmd, stdin: stdin, stdout: stdout,
		messages: make(chan received, 1), stop: make(chan struct{}),
		readDone: make(chan struct{}), waitDone: make(chan error, 1),
	}
	go func() { c.waitDone <- cmd.Wait() }()
	go c.read()
	return c, nil
}

func (c *client) close() {
	close(c.stop)
	c.stdin.Close()
	select {
	case <-c.waitDone:
	case <-time.After(shutdownGrace):
		c.cmd.Process.Kill()
		<-c.waitDone
	}
	c.stdout.Close()
	<-c.readDone
}

func (c *client) read() {
	defer close(c.readDone)
	defer close(c.messages)
	scanner := bufio.NewScanner(c.stdout)
	scanner.Buffer(make([]byte, 4096), maxMessageSize)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var msg wireMessage
		if line[0] != '{' || json.Unmarshal(line, &msg) != nil {
			c.deliver(received{err: errors.New("Codex app-server returned malformed JSON")})
			return
		}
		if !c.deliver(received{message: msg}) {
			return
		}
	}
	if scanner.Err() != nil {
		c.deliver(received{err: errors.New("could not read Codex app-server response (I/O error or message exceeds 4 MiB)")})
	} else {
		c.deliver(received{err: errors.New("Codex app-server closed its output before responding; check your Codex installation")})
	}
}

func (c *client) deliver(msg received) bool {
	select {
	case c.messages <- msg:
		return true
	case <-c.stop:
		return false
	}
}

func (c *client) write(ctx context.Context, message any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := json.Marshal(message)
	if err != nil {
		return errors.New("could not encode Codex app-server request")
	}
	data = append(data, '\n')
	written := make(chan error, 1)
	go func() {
		_, err := c.stdin.Write(data)
		written <- err
	}()
	select {
	case err := <-written:
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return errors.New("could not write to Codex app-server")
		}
		return nil
	case <-ctx.Done():
		// Closing also unblocks a write if the subprocess stopped consuming stdin.
		c.stdin.Close()
		<-written
		return ctx.Err()
	}
}

func (c *client) call(ctx context.Context, method string, params any, result any) error {
	c.nextID++
	id := c.nextID
	if err := c.write(ctx, struct {
		ID     int64  `json:"id"`
		Method string `json:"method"`
		Params any    `json:"params"`
	}{id, method, params}); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case incoming, ok := <-c.messages:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !ok {
				return errors.New("Codex app-server connection closed")
			}
			if incoming.err != nil {
				return incoming.err
			}
			msg := incoming.message
			if msg.Method != "" {
				if msg.Result != nil || msg.Error != nil {
					return errors.New("Codex app-server returned an invalid protocol message")
				}
				if len(msg.ID) > 0 {
					if !validRequestID(msg.ID) {
						return errors.New("Codex app-server sent an invalid request ID")
					}
					if err := c.write(ctx, map[string]any{
						"id":    msg.ID,
						"error": map[string]any{"code": -32601, "message": "ccodex does not support server-initiated requests"},
					}); err != nil {
						return err
					}
				}
				continue // Account updates and other notifications are unsolicited.
			}
			if string(msg.ID) != strconv.FormatInt(id, 10) {
				return errors.New("Codex app-server returned a response with an unexpected request ID")
			}
			if (msg.Error != nil) == (msg.Result != nil) {
				return errors.New("Codex app-server returned an invalid response")
			}
			if msg.Error != nil {
				return msg.Error
			}
			payload := bytes.TrimSpace(msg.Result)
			if len(payload) == 0 || payload[0] != '{' || json.Unmarshal(payload, result) != nil {
				return errors.New("Codex app-server returned an invalid result; update Codex CLI and try again")
			}
			return nil
		}
	}
}

func validRequestID(id json.RawMessage) bool {
	var number int64
	if len(id) == 0 || bytes.Equal(id, []byte("null")) {
		return false
	}
	if id[0] == '"' {
		var text string
		return json.Unmarshal(id, &text) == nil
	}
	return json.Unmarshal(id, &number) == nil
}
