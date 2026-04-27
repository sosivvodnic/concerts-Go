package api

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"time"

	"concerts-go/internal/httpx"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

type reservationSeatReq struct {
	Row  int64 `json:"row"`
	Seat int   `json:"seat"`
}

type postReservationReq struct {
	ReservationToken *string              `json:"reservation_token"`
	Reservations     []reservationSeatReq `json:"reservations"`
	Duration         *int                 `json:"duration"`
}

func (s *Server) postReservation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	concertID, ok1 := parseID(chi.URLParam(r, "concert-id"))
	showID, ok2 := parseID(chi.URLParam(r, "show-id"))
	if !ok1 || !ok2 {
		httpx.WriteError(w, http.StatusNotFound, "A concert or show with this ID does not exist")
		return
	}

	var req postReservationReq
	if err := httpx.ReadJSON(r, &req); err != nil {
		httpx.WriteError(w, http.StatusUnprocessableEntity, "Validation failed")
		return
	}

	fields := map[string]string{}
	if req.Reservations == nil {
		fields["reservations"] = "The reservations field is required."
	}

	duration := 300
	if req.Duration != nil {
		duration = *req.Duration
	}
	if duration < 1 || duration > 300 {
		fields["duration"] = "The duration must be between 1 and 300."
	}

	if len(fields) > 0 {
		httpx.WriteJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  "Validation failed",
			"fields": fields,
		})
		return
	}

	// Validate show belongs to concert
	var exists bool
	if err := s.DB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM shows WHERE id=$1 AND concert_id=$2)`, showID, concertID).Scan(&exists); err != nil || !exists {
		httpx.WriteError(w, http.StatusNotFound, "A concert or show with this ID does not exist")
		return
	}

	reservedUntil := time.Now().UTC().Add(time.Duration(duration) * time.Second)

	tx, err := s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// cleanup expired reservations (free seats)
	if _, err := tx.Exec(ctx, `
UPDATE location_seats
SET reservation_id = NULL
WHERE reservation_id IN (SELECT id FROM reservations WHERE expires_at < now())`); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := tx.Exec(ctx, `DELETE FROM reservations WHERE expires_at < now()`); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	var reservationID int64
	var token string

	if req.ReservationToken != nil && *req.ReservationToken != "" {
		token = *req.ReservationToken
		err := tx.QueryRow(ctx, `SELECT id FROM reservations WHERE token=$1`, token).Scan(&reservationID)
		if err != nil {
			httpx.WriteError(w, http.StatusForbidden, "Invalid reservation token")
			return
		}
	} else {
		token = newToken()
		err := tx.QueryRow(ctx, `INSERT INTO reservations(token, expires_at) VALUES ($1, $2) RETURNING id`, token, reservedUntil).Scan(&reservationID)
		if err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "internal error")
			return
		}
	}

	// clear previous reservation seats for this show (same token)
	if _, err := tx.Exec(ctx, `
UPDATE location_seats s
SET reservation_id = NULL
FROM location_seat_rows r
WHERE s.location_seat_row_id = r.id
  AND r.show_id = $1
  AND s.reservation_id = $2`, showID, reservationID); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// If empty array: just extend/keep reservation
	if len(req.Reservations) == 0 {
		if _, err := tx.Exec(ctx, `UPDATE reservations SET expires_at=$1 WHERE id=$2`, reservedUntil, reservationID); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if err := tx.Commit(ctx); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "internal error")
			return
		}
		httpx.WriteJSON(w, http.StatusCreated, map[string]any{
			"reserved":          true,
			"reservation_token": token,
			"reserved_until":    reservedUntil.Format(time.RFC3339),
		})
		return
	}

	// validate and reserve seats
	for _, rs := range req.Reservations {
		if rs.Row <= 0 {
			fields["reservations"] = "The row field is required."
			break
		}
		if rs.Seat <= 0 {
			fields["reservations"] = "The seat field is required."
			break
		}

		var rowOK bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM location_seat_rows WHERE id=$1 AND show_id=$2)`, rs.Row, showID).Scan(&rowOK); err != nil || !rowOK {
			fields["reservations"] = fmt.Sprintf("Seat %d in row %d is invalid.", rs.Seat, rs.Row)
			break
		}

		var (
			seatID         int64
			existingResID  *int64
			existingTicket *int64
		)

		err := tx.QueryRow(ctx, `
SELECT id, reservation_id, ticket_id
FROM location_seats
WHERE location_seat_row_id=$1 AND number=$2
FOR UPDATE`, rs.Row, rs.Seat).Scan(&seatID, &existingResID, &existingTicket)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				fields["reservations"] = fmt.Sprintf("Seat %d in row %d is invalid.", rs.Seat, rs.Row)
				break
			}
			httpx.WriteError(w, http.StatusInternalServerError, "internal error")
			return
		}

		if existingTicket != nil {
			fields["reservations"] = fmt.Sprintf("Seat %d in row %d is already taken.", rs.Seat, rs.Row)
			break
		}

		if existingResID != nil && *existingResID != reservationID {
			var otherExpires time.Time
			err := tx.QueryRow(ctx, `SELECT expires_at FROM reservations WHERE id=$1`, *existingResID).Scan(&otherExpires)
			if err == nil && otherExpires.After(time.Now()) {
				fields["reservations"] = fmt.Sprintf("Seat %d in row %d is already taken.", rs.Seat, rs.Row)
				break
			}
			// expired or missing -> free
			if _, err := tx.Exec(ctx, `UPDATE location_seats SET reservation_id=NULL WHERE id=$1`, seatID); err != nil {
				httpx.WriteError(w, http.StatusInternalServerError, "internal error")
				return
			}
		}

		if _, err := tx.Exec(ctx, `UPDATE location_seats SET reservation_id=$1 WHERE id=$2`, reservationID, seatID); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "internal error")
			return
		}
	}

	if len(fields) > 0 {
		httpx.WriteJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"error":  "Validation failed",
			"fields": fields,
		})
		return
	}

	if _, err := tx.Exec(ctx, `UPDATE reservations SET expires_at=$1 WHERE id=$2`, reservedUntil, reservationID); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if err := tx.Commit(ctx); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	httpx.WriteJSON(w, http.StatusCreated, map[string]any{
		"reserved":          true,
		"reservation_token": token,
		"reserved_until":    reservedUntil.Format(time.RFC3339),
	})
}

func newToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		// fallback time-based (should be extremely rare)
		return fmt.Sprintf("tok-%d", time.Now().UnixNano())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

