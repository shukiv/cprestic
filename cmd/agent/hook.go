package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/shuki/cprest/internal/hookspool"
)

type cpanelHookDescriptor struct {
	Blocking int    `json:"blocking,omitempty"`
	Category string `json:"category"`
	Event    string `json:"event"`
	Stage    string `json:"stage"`
	Hook     string `json:"hook"`
	ExecType string `json:"exectype"`
}

// writeCPanelHookDescription implements the --describe protocol required by
// manage_hooks. cPanel refuses to make a manually registered hook blocking.
func writeCPanelHookDescription(w io.Writer) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find hook executable: %w", err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return fmt.Errorf("make hook executable absolute: %w", err)
	}
	descriptors := []cpanelHookDescriptor{
		{Category: "Whostmgr", Event: "Accounts::Create", Stage: "post", Hook: executable + " --cpanel-hook=create", ExecType: "script"},
		{Category: "Whostmgr", Event: "Accounts::Modify", Stage: "post", Hook: executable + " --cpanel-hook=modify", ExecType: "script"},
		{Category: "Whostmgr", Event: "Accounts::suspendacct", Stage: "post", Hook: executable + " --cpanel-hook=suspend", ExecType: "script"},
		{Category: "Whostmgr", Event: "Accounts::unsuspendacct", Stage: "post", Hook: executable + " --cpanel-hook=unsuspend", ExecType: "script"},
		{Blocking: 1, Category: "Whostmgr", Event: "Accounts::Remove", Stage: "pre", Hook: executable + " --cpanel-hook=remove-pre", ExecType: "script"},
		{Category: "Whostmgr", Event: "Accounts::Remove", Stage: "post", Hook: executable + " --cpanel-hook=remove", ExecType: "script"},
	}
	return json.NewEncoder(w).Encode(descriptors)
}

type hookServiceError struct {
	StatusCode int
	Detail     string
}

func (e *hookServiceError) Error() string {
	return fmt.Sprintf("cprest service returned HTTP %d: %s", e.StatusCode, e.Detail)
}

// blockingHookFailure distinguishes a policy refusal from service
// unavailability. Client errors mean the service reached a definite decision
// and must block; network and server errors fail open in main.
func blockingHookFailure(err error) (string, bool) {
	var serviceErr *hookServiceError
	if !errors.As(err, &serviceErr) {
		return "", false
	}
	return serviceErr.Detail, serviceErr.StatusCode >= 400 && serviceErr.StatusCode < 500
}

// serviceAnswered reports whether the failure came back from a service that
// was actually reachable.
//
// The distinction matters for every hook, not only the blocking one. A
// service that answered has a decision worth reporting to WHM; a service
// that could not be reached is restarting, being upgraded, or stopped for
// maintenance, and reporting that as a hook failure makes ordinary account
// administration look broken across the whole server.
func serviceAnswered(err error) bool {
	var serviceErr *hookServiceError
	return errors.As(err, &serviceErr)
}

func hookMessage(detail string) string {
	detail = strings.Join(strings.Fields(detail), " ")
	detail = strings.ReplaceAll(detail, "BAILOUT", "")
	detail = strings.TrimSpace(detail)
	if detail == "" {
		return "the backup safety check refused the request"
	}
	runes := []rune(detail)
	if len(runes) > 500 {
		detail = string(runes[:500]) + "…"
	}
	return detail
}

// runCPanelHook forwards the JSON line supplied by cPanel's Standardized
// Hooks system to the already-running standalone service. The hook process
// never opens the bbolt file alongside the service.
//
// It returns what cPanel sent as well as the outcome, so a caller whose
// service could not be reached can write the event down instead of
// losing it.
func runCPanelHook(socketPath, event string) ([]byte, error) {
	if event != "create" && event != "modify" && event != "suspend" &&
		event != "unsuspend" && event != "remove" && event != "remove-pre" {
		return nil, fmt.Errorf("unknown cPanel hook event %q", event)
	}
	limited := io.LimitReader(os.Stdin, (1<<20)+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read cPanel hook input: %w", err)
	}
	if len(body) > 1<<20 {
		return nil, fmt.Errorf("cPanel hook input is larger than 1 MiB")
	}

	dialer := &net.Dialer{Timeout: 5 * time.Second}
	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socketPath)
		}},
	}
	requestURL := "http://unix/event?event=" + url.QueryEscape(event)
	request, err := http.NewRequest(http.MethodPost, requestURL, bytes.NewReader(body))
	if err != nil {
		return body, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return body, fmt.Errorf("notify cprest service: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return body, &hookServiceError{
			StatusCode: response.StatusCode, Detail: string(bytes.TrimSpace(detail))}
	}
	return body, nil
}

// spoolCPanelHook writes down an account event the service was not there
// to hear.
//
// Only creation and removal are kept, and only these two need to be: they
// are what says a username has changed hands, and a username recreated
// onto the same uid while the service was down is indistinguishable from
// the account that was there before. Everything else polling can work out
// again by looking.
// The time is when cPanel ran the hook, not when the spool was written.
// A service that is stopped rather than absent takes the socket timeout to
// say so, and a boundary recorded thirty seconds late is thirty seconds of
// the new owner's backups filed under the old one.
func spoolCPanelHook(dir, event string, payload []byte, at time.Time) (string, error) {
	account := hookspool.AccountIn(payload)
	if account == "" {
		return "", fmt.Errorf("the %s hook did not name an account", event)
	}
	return hookspool.Write(dir, hookspool.Event{
		At: at.UTC(), Event: event, Account: account, Payload: payload,
	})
}
