package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// EnquiryMaxLen is the longest message cloud.enquiries accepts, in
// characters. Migration 0016's CHECK holds the same number.
const EnquiryMaxLen = 4000

// EnquiryDailyCap is how many enquiries CreateEnquiry stores in any 24 hours:
// at most 8 MB of message text, at EnquiryMaxLen characters of up to 4 bytes.
const EnquiryDailyCap = 500

// ErrEnquiryCap reports that EnquiryDailyCap enquiries arrived in the last 24
// hours.
var ErrEnquiryCap = errors.New("the enquiry form is taking no more messages today")

// Enquiry is one message from the form on the marketing site.
type Enquiry struct {
	ID        int64
	Email     string
	Message   string
	CreatedAt time.Time
}

// CreateEnquiry stores a message and returns its id, or ErrEnquiryCap.
//
// email is folded by NormalizeEmail. The caller validates both fields; a value
// migration 0016's CHECKs refuse returns their error.
func (s *Store) CreateEnquiry(ctx context.Context, email, message string) (int64, error) {
	// Each statement counts without the rows other statements have not yet
	// committed, so concurrent calls can pass the cap by at most the pool's
	// connection count.
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO cloud.enquiries (email, message)
		SELECT $1, $2
		WHERE (SELECT count(*) FROM cloud.enquiries
		       WHERE created_at > now() - interval '24 hours') < $3
		RETURNING id`,
		NormalizeEmail(email), message, EnquiryDailyCap).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrEnquiryCap
	}
	return id, err
}

// Enquiries returns every stored enquiry, newest first.
func (s *Store) Enquiries(ctx context.Context) ([]Enquiry, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, email, message, created_at FROM cloud.enquiries ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Enquiry
	for rows.Next() {
		var e Enquiry
		if err := rows.Scan(&e.ID, &e.Email, &e.Message, &e.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
