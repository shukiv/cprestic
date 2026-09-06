package controller

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/shuki/cprest/internal/certs"
	"github.com/shuki/cprest/internal/protocol"
	"github.com/shuki/cprest/internal/store"
)

// API serves the agent-facing endpoints.
//
// Every route except the health check requires a client certificate whose
// fingerprint is registered against a server. Authorisation is by
// fingerprint, not by certificate subject: a name is a label, a pinned
// fingerprint is an identity.
type API struct {
	service *Service
	log     *slog.Logger

	// LongPollWait is how long a job request waits for work before
	// answering "nothing yet". Agents re-poll immediately afterwards.
	LongPollWait time.Duration
	// PollBackoff is the interval between database checks while waiting.
	PollBackoff time.Duration
}

// NewAPI builds the agent API.
func NewAPI(service *Service, log *slog.Logger) *API {
	if log == nil {
		log = slog.Default()
	}
	return &API{
		service:      service,
		log:          log,
		LongPollWait: 30 * time.Second,
		PollBackoff:  time.Second,
	}
}

// Handler returns the routed handler.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+protocol.PathHealthz, a.handleHealthz)
	mux.Handle("POST "+protocol.PathEnrol, a.authenticated(a.handleEnrol))
	mux.Handle("GET "+protocol.PathNextJob, a.authenticated(a.handleNextJob))
	mux.Handle("POST "+protocol.PathReport, a.authenticated(a.handleReport))
	mux.Handle("POST "+protocol.PathRestoreReport, a.authenticated(a.handleRestoreReport))
	mux.Handle("POST "+protocol.PathRenewLease, a.authenticated(a.handleRenewLease))
	return mux
}

// ServerTLSConfig builds a TLS configuration that requires and verifies
// agent client certificates.
func ServerTLSConfig(serverCert tls.Certificate, clientCAs *x509.CertPool) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientCAs:    clientCAs,
		// Anonymous requests never reach a handler.
		ClientAuth: tls.RequireAndVerifyClientCert,
		MinVersion: tls.VersionTLS13,
	}
}

type contextKey string

const serverContextKey contextKey = "cprest.server"

// authenticated resolves the client certificate to a registered server.
func (a *API) authenticated(next func(http.ResponseWriter, *http.Request, store.Server)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
			writeError(w, http.StatusUnauthorized, "client certificate required")
			return
		}
		fingerprint := certs.Fingerprint(r.TLS.PeerCertificates[0])

		server, err := a.service.store.ServerByFingerprint(r.Context(), fingerprint)
		if errors.Is(err, store.ErrNotFound) {
			// A valid certificate from the CA is not enough: the
			// fingerprint must be registered against a server.
			a.log.Warn("rejected unregistered agent certificate", "fingerprint", fingerprint)
			writeError(w, http.StatusForbidden, "certificate is not registered")
			return
		}
		if err != nil {
			a.log.Error("authenticate agent", "error", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if server.Status == "suspended" || server.Status == "retired" {
			writeError(w, http.StatusForbidden, "server is not active")
			return
		}

		ctx := context.WithValue(r.Context(), serverContextKey, server)
		next(w, r.WithContext(ctx), server)
	})
}

func (a *API) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *API) handleEnrol(w http.ResponseWriter, r *http.Request, server store.Server) {
	var req protocol.EnrolRequest
	if !decodeBody(w, r, &req) {
		return
	}
	response, err := a.service.Enrol(r.Context(), server.ID, req)
	if err != nil {
		a.log.Error("enrol", "server_id", server.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "enrolment failed")
		return
	}
	writeJSON(w, http.StatusOK, response)
}

// handleNextJob long-polls for work.
//
// Holding the request open avoids a fleet of agents hammering the
// controller once a second, while still delivering a job within a second
// of it being scheduled.
func (a *API) handleNextJob(w http.ResponseWriter, r *http.Request, server store.Server) {
	ctx, cancel := context.WithTimeout(r.Context(), a.LongPollWait)
	defer cancel()

	for {
		assignment, err := a.service.NextWork(ctx, server.ID)
		switch {
		case err == nil:
			a.logDispatch(server, assignment)
			writeJSON(w, http.StatusOK, assignment)
			return
		case errors.Is(err, store.ErrNoWork):
			// Keep waiting.
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			w.WriteHeader(http.StatusNoContent)
			return
		default:
			a.log.Error("claim job", "server_id", server.ID, "error", err)
			writeError(w, http.StatusInternalServerError, "could not claim a job")
			return
		}

		select {
		case <-ctx.Done():
			w.WriteHeader(http.StatusNoContent)
			return
		case <-time.After(a.PollBackoff):
		}
	}
}

func (a *API) logDispatch(server store.Server, assignment protocol.Assignment) {
	switch {
	case assignment.Backup != nil:
		a.log.Info("backup dispatched",
			"server_id", server.ID, "job_id", assignment.Backup.JobID,
			"account", assignment.Backup.CPanelUser,
			"targets", len(assignment.Backup.Targets))
	case assignment.Restore != nil:
		a.log.Info("restore dispatched",
			"server_id", server.ID, "job_id", assignment.Restore.JobID,
			"account", assignment.Restore.CPanelUser,
			"snapshot", assignment.Restore.SnapshotID,
			"kind", assignment.Restore.Kind, "apply", assignment.Restore.Apply)
	}
}

func (a *API) handleRestoreReport(w http.ResponseWriter, r *http.Request, server store.Server) {
	var report protocol.RestoreReport
	if !decodeBody(w, r, &report) {
		return
	}
	if err := a.service.ReportRestore(r.Context(), server.ID, report); err != nil {
		a.log.Error("apply restore report",
			"server_id", server.ID, "job_id", report.JobID, "error", err)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": report.Status})
}

// handleRenewLease answers an agent that is still working. A renewal that
// is refused is not an error the agent should retry: it means the job is
// no longer theirs, and 409 says so distinctly from a controller that is
// merely unwell.
func (a *API) handleRenewLease(w http.ResponseWriter, r *http.Request, server store.Server) {
	var req protocol.LeaseRenewal
	if !decodeBody(w, r, &req) {
		return
	}
	renewed, err := a.service.RenewLease(r.Context(), server.ID, req)
	if errors.Is(err, store.ErrNotFound) {
		a.log.Warn("refused a lease renewal for work this agent no longer holds",
			"server_id", server.ID, "job_id", req.JobID, "restore", req.Restore)
		writeError(w, http.StatusConflict, "this job is not leased to you")
		return
	}
	if err != nil {
		a.log.Error("renew lease", "server_id", server.ID, "job_id", req.JobID, "error", err)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, renewed)
}

func (a *API) handleReport(w http.ResponseWriter, r *http.Request, server store.Server) {
	var report protocol.JobReport
	if !decodeBody(w, r, &report) {
		return
	}
	status, err := a.service.Report(r.Context(), server.ID, report)
	if err != nil {
		a.log.Error("apply report", "server_id", server.ID, "job_id", report.JobID, "error", err)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": string(status)})
}

func decodeBody(w http.ResponseWriter, r *http.Request, target any) bool {
	body := http.MaxBytesReader(w, r.Body, protocol.MaxBodyBytes)
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		if errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "request body is empty")
		} else {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
		}
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, protocol.ErrorResponse{Error: message})
}
