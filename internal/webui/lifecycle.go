package webui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/shuki/cprest/internal/nodestore"
)

// ListenLifecycle receives cPanel Standardized Hook notifications. It has
// its own root-only socket so hook requests never pass through the WHM CGI
// and do not need the browser's CSRF token.
func (s *Server) ListenLifecycle(ctx context.Context, socketPath string) error {
	if err := prepareSocketDir(filepath.Dir(socketPath), 0o700); err != nil {
		return err
	}
	if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("webui: remove stale lifecycle socket: %w", err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("webui: listen on %s: %w", socketPath, err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		_ = listener.Close()
		return fmt.Errorf("webui: secure lifecycle socket: %w", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /event", s.handleLifecycleEvent)
	server := &http.Server{
		Handler: s.recoverPanics(mux), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second,
		MaxHeaderBytes: 8 << 10,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	s.log.Info("cPanel lifecycle hook listening", "socket", socketPath)
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) handleLifecycleEvent(w http.ResponseWriter, r *http.Request) {
	event := r.URL.Query().Get("event")
	record := func(account string, ok bool, detail string) error {
		_, err := s.engine.Store().PutLifecycleEvent(nodestore.LifecycleEvent{
			Event: event, Account: account, OK: ok, Detail: detail,
		})
		return err
	}
	fail := func(account string, status int, err error) {
		if recordErr := record(account, false, err.Error()); recordErr != nil {
			s.log.Error("record failed cPanel lifecycle hook", "error", recordErr)
		}
		http.Error(w, err.Error(), status)
	}
	if event != "create" && event != "modify" && event != "suspend" &&
		event != "unsuspend" && event != "remove" && event != "remove-pre" {
		fail("", http.StatusBadRequest, errors.New("unknown lifecycle event"))
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		fail("", http.StatusBadRequest, errors.New("unreadable hook payload"))
		return
	}
	account := lifecycleAccount(raw)
	if event == "create" || event == "suspend" || event == "unsuspend" || event == "remove" {
		if account == "" {
			fail("", http.StatusBadRequest, fmt.Errorf("%s hook did not name an account", event))
			return
		}
	}
	if event == "remove-pre" {
		decision, err := s.engine.AccountRemovalSafety(account, time.Now().UTC())
		if err != nil {
			fail(account, http.StatusInternalServerError, err)
			return
		}
		if !decision.Allowed {
			if err := record(account, false, decision.Detail); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			http.Error(w, decision.Detail, http.StatusConflict)
			return
		}
		if err := record(account, true, decision.Detail); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if event == "create" {
		if err := s.engine.AccountCreated(account); err != nil {
			fail(account, http.StatusInternalServerError, err)
			return
		}
		queued, err := s.engine.QueueInitialBackup(account)
		if err != nil {
			fail(account, http.StatusInternalServerError, err)
			return
		}
		detail := "ownership boundary recorded; no enabled all-account policy"
		if queued {
			detail = "ownership boundary recorded; initial backup queued"
		}
		if err := record(account, true, detail); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if event == "suspend" {
		result, err := s.engine.QueueSuspensionBackup(account)
		if err != nil {
			fail(account, http.StatusInternalServerError, err)
			return
		}
		detail := "account suspended; preservation backups are disabled"
		switch {
		case result.Busy:
			detail = "account suspended; work is already queued or running, so no duplicate backup was added"
		case result.Queued:
			names := make([]string, 0, len(result.Policies))
			for _, policy := range result.Policies {
				names = append(names, policy.Name)
			}
			detail = "account suspended; preservation backup queued with " + strings.Join(names, ", ")
		case result.Enabled:
			detail = "account suspended; no enabled full-account policy covers it"
		}
		if err := record(account, true, detail); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if event == "unsuspend" {
		if err := record(account, true, "account unsuspended"); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if event == "remove" {
		if err := s.engine.AccountRemoved(account); err != nil {
			fail(account, http.StatusInternalServerError, err)
			return
		}
	}
	var reconcileErr error
	if event == "modify" {
		reconcileErr = s.engine.ReconcileAccountRenames(r.Context())
	} else {
		reconcileErr = s.engine.ReconcileAccounts(r.Context())
	}
	if reconcileErr != nil {
		fail(account, http.StatusInternalServerError, reconcileErr)
		return
	}
	detail := "account registry reconciled"
	if event == "remove" {
		detail = "identity retired; named policies reconciled"
	}
	if err := record(account, true, detail); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// lifecycleAccount tolerates both the context/data envelope used by
// Standardized Hooks and the slightly different return shapes of WHM API
// account functions. Only a cPanel-safe username is accepted.
func lifecycleAccount(raw []byte) string {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	var walk func(any) string
	walk = func(value any) string {
		switch typed := value.(type) {
		case map[string]any:
			if data, ok := typed["data"]; ok {
				if found := walk(data); found != "" {
					return found
				}
			}
			for _, key := range []string{"user", "username", "cpuser"} {
				if candidate, ok := typed[key].(string); ok && safeCPanelUser(candidate) {
					return candidate
				}
			}
			for key, child := range typed {
				if key == "data" {
					continue
				}
				if found := walk(child); found != "" {
					return found
				}
			}
		case []any:
			for _, child := range typed {
				if found := walk(child); found != "" {
					return found
				}
			}
		}
		return ""
	}
	return walk(value)
}

func safeCPanelUser(value string) bool {
	if value == "" || len(value) > 16 {
		return false
	}
	for _, char := range value {
		if !((char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == '_') {
			return false
		}
	}
	return !strings.HasPrefix(value, "_")
}
