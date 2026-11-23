package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/rachitkumar205/atlantis/internal/cloud/store"
)

// enquiryReceived is the answer to a stored enquiry, and to one the honeypot
// caught.
const enquiryReceived = "Thanks. Your message was sent."

type enquiryRequest struct {
	Email   string `json:"email"`
	Message string `json:"message"`

	// Website is the form's honeypot: a field hidden from people, which a bot
	// filling every input fills too.
	Website string `json:"website"`
}

// handleEnquiry stores a message from the form on the marketing site, at
// cfg.SiteOrigin.
func (s *Server) handleEnquiry(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Vary", "Origin")

	// The check stops other sites' pages from posting through a visitor's
	// browser. A client outside a browser sends any Origin it likes.
	if originOf(r.Header.Get("Origin")) != s.cfg.SiteOrigin {
		jsonError(w, "this form accepts messages from "+s.cfg.SiteOrigin+" only", http.StatusForbidden)
		return
	}

	// Every answer below carries the header. A 400 or a 429 without it
	// reaches the page as a network error, and the site shows a generic
	// failure in place of the reason.
	w.Header().Set("Access-Control-Allow-Origin", s.cfg.SiteOrigin)

	if !s.rateLimitedBy(s.enquiryLim, w, r) {
		return
	}

	var req enquiryRequest
	if !decode(w, r, &req) {
		return
	}

	// A caught bot gets the answer a stored message gets.
	if req.Website != "" {
		s.log.Info("enquiry dropped: honeypot filled")
		writeJSON(w, http.StatusOK, map[string]string{"message": enquiryReceived})
		return
	}

	// validEmail refuses a NUL, which Postgres refuses in text with an error
	// that would surface as a 500.
	email := store.NormalizeEmail(req.Email)
	if !validEmail(email) {
		jsonError(w, "that does not look like an email address", http.StatusBadRequest)
		return
	}

	// A textarea submits CRLF line breaks and its maxlength counts each as
	// one character, so the length is measured after folding them.
	message := strings.TrimSpace(strings.ReplaceAll(req.Message, "\r\n", "\n"))
	switch {
	case message == "":
		jsonError(w, "write a message", http.StatusBadRequest)
		return
	case utf8.RuneCountInString(message) > store.EnquiryMaxLen:
		jsonError(w, fmt.Sprintf("keep the message to %d characters", store.EnquiryMaxLen), http.StatusBadRequest)
		return
	case strings.ContainsRune(message, 0):
		jsonError(w, "the message contains a character that cannot be stored", http.StatusBadRequest)
		return
	}

	id, err := s.db.CreateEnquiry(r.Context(), email, message)
	if errors.Is(err, store.ErrEnquiryCap) {
		s.log.Warn("enquiry refused: daily cap reached", "cap", store.EnquiryDailyCap)
		jsonError(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		s.log.Error("store enquiry", "err", err)
		jsonError(w, "could not save the message", http.StatusInternalServerError)
		return
	}
	s.log.Info("enquiry stored", "id", id)
	writeJSON(w, http.StatusOK, map[string]string{"message": enquiryReceived})
}

// handleEnquiryPreflight admits the site's JSON POST. Any other origin gets
// no CORS headers, and its browser does not send the request.
func (s *Server) handleEnquiryPreflight(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Add("Vary", "Origin")
	if originOf(r.Header.Get("Origin")) == s.cfg.SiteOrigin {
		h.Set("Access-Control-Allow-Origin", s.cfg.SiteOrigin)
		h.Set("Access-Control-Allow-Methods", "POST")
		h.Set("Access-Control-Allow-Headers", "Content-Type")
		h.Set("Access-Control-Max-Age", "600")
	}
	w.WriteHeader(http.StatusNoContent)
}
