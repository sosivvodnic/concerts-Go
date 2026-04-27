package api

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"concerts-go/internal/httpx"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

type postBookingReq struct {
	ReservationToken string `json:"reservation_token"`
	Name             string `json:"name"`
	Address          string `json:"address"`
	City             string `json:"city"`
	Zip              string `json:"zip"`
	Country          string `json:"country"`
}

type postTicketsReq struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

type ticketDTO struct {
	ID        int64  `json:"id"`
	Code      string `json:"code"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	Row       struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"row"`
	Seat int `json:"seat"`
	Show struct {
		ID    int64  `json:"id"`
		Start string `json:"start"`
		End   string `json:"end"`
		Concert struct {
			ID       int64  `json:"id"`
			Artist   string `json:"artist"`
			Location struct {
				ID   int64  `json:"id"`
				Name string `json:"name"`
			} `json:"location"`
		} `json:"concert"`
	} `json:"show"`
}

func (s *Server) postBooking(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	concertID, ok1 := parseID(chi.URLParam(r, "concert-id"))
	showID, ok2 := parseID(chi.URLParam(r, "show-id"))
	if !ok1 || !ok2 {
		httpx.WriteError(w, http.StatusNotFound, "A concert or show with this ID does not exist")
		return
	}

	var req postBookingReq
	if err := httpx.ReadJSON(r, &req); err != nil {
		httpx.WriteError(w, http.StatusUnprocessableEntity, "Validation failed")
		return
	}

	fields := map[string]string{}
	if req.ReservationToken == "" {
		fields["reservation_token"] = "The reservation token field is required."
	}
	if req.Name == "" {
		fields["name"] = "The name field is required."
	}
	if req.Address == "" {
		fields["address"] = "The address field is required."
	}
	if req.City == "" {
		fields["city"] = "The city field is required."
	}
	if req.Zip == "" {
		fields["zip"] = "The zip field is required."
	}
	if req.Country == "" {
		fields["country"] = "The country field is required."
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

	tx, err := s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// cleanup expired reservations
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
	var expiresAt time.Time
	err = tx.QueryRow(ctx, `SELECT id, expires_at FROM reservations WHERE token=$1`, req.ReservationToken).Scan(&reservationID, &expiresAt)
	if err != nil || !expiresAt.After(time.Now()) {
		httpx.WriteError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	// find reserved seats for this show
	type seatRef struct {
		seatID int64
		rowID  int64
		rowName string
		number int
	}
	seatRows, err := tx.Query(ctx, `
SELECT s.id, r.id, r.name, s.number
FROM location_seats s
JOIN location_seat_rows r ON r.id = s.location_seat_row_id
WHERE r.show_id = $1 AND s.reservation_id = $2
ORDER BY r."order", s.number
FOR UPDATE`, showID, reservationID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer seatRows.Close()

	seats := make([]seatRef, 0)
	for seatRows.Next() {
		var sr seatRef
		if err := seatRows.Scan(&sr.seatID, &sr.rowID, &sr.rowName, &sr.number); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "internal error")
			return
		}
		seats = append(seats, sr)
	}
	if seatRows.Err() != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if len(seats) == 0 {
		httpx.WriteError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	// create booking
	var bookingID int64
	if err := tx.QueryRow(ctx, `
INSERT INTO bookings(name, address, city, zip, country)
VALUES ($1,$2,$3,$4,$5)
RETURNING id`, req.Name, req.Address, req.City, req.Zip, req.Country).Scan(&bookingID); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// fetch show + concert + location data for response
	var (
		showStart time.Time
		showEnd   time.Time
		artist    string
		locID     int64
		locName   string
	)
	if err := tx.QueryRow(ctx, `
SELECT sh.start, sh."end", c.artist, l.id, l.name
FROM shows sh
JOIN concerts c ON c.id = sh.concert_id
JOIN locations l ON l.id = c.location_id
WHERE sh.id=$1`, showID).Scan(&showStart, &showEnd, &artist, &locID, &locName); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	outTickets := make([]ticketDTO, 0, len(seats))
	for _, sr := range seats {
		code := newTicketCode()
		var ticketID int64
		var createdAt time.Time
		if err := tx.QueryRow(ctx, `
INSERT INTO tickets(code, booking_id)
VALUES ($1, $2)
RETURNING id, created_at`, code, bookingID).Scan(&ticketID, &createdAt); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "internal error")
			return
		}

		if _, err := tx.Exec(ctx, `UPDATE location_seats SET ticket_id=$1, reservation_id=NULL WHERE id=$2`, ticketID, sr.seatID); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "internal error")
			return
		}

		var td ticketDTO
		td.ID = ticketID
		td.Code = code
		td.Name = req.Name
		td.CreatedAt = createdAt.UTC().Format(time.RFC3339)
		td.Row.ID = sr.rowID
		td.Row.Name = sr.rowName
		td.Seat = sr.number
		td.Show.ID = showID
		td.Show.Start = showStart.UTC().Format(time.RFC3339)
		td.Show.End = showEnd.UTC().Format(time.RFC3339)
		td.Show.Concert.ID = concertID
		td.Show.Concert.Artist = artist
		td.Show.Concert.Location.ID = locID
		td.Show.Concert.Location.Name = locName
		outTickets = append(outTickets, td)
	}

	// reservation no longer needed
	if _, err := tx.Exec(ctx, `DELETE FROM reservations WHERE id=$1`, reservationID); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if err := tx.Commit(ctx); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	httpx.WriteJSON(w, http.StatusCreated, map[string]any{"tickets": outTickets})
}

func (s *Server) postTickets(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	var req postTicketsReq
	if err := httpx.ReadJSON(r, &req); err != nil {
		httpx.WriteError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}
	if req.Code == "" || req.Name == "" {
		httpx.WriteError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	// Validate code + name and get booking_id
	var bookingID int64
	err := s.DB.QueryRow(ctx, `
SELECT b.id
FROM tickets t
JOIN bookings b ON b.id = t.booking_id
WHERE t.code=$1 AND b.name=$2`, req.Code, req.Name).Scan(&bookingID)
	if err != nil {
		httpx.WriteError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	tickets, err := s.fetchTicketsByBooking(ctx, bookingID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{"tickets": tickets})
}

func (s *Server) postTicketCancel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ticketIDStr := chi.URLParam(r, "ticket-id")
	ticketID, err := strconv.ParseInt(ticketIDStr, 10, 64)
	if err != nil || ticketID <= 0 {
		httpx.WriteError(w, http.StatusNotFound, "A ticket with this ID does not exist")
		return
	}

	var req postTicketsReq
	if err := httpx.ReadJSON(r, &req); err != nil || req.Code == "" || req.Name == "" {
		httpx.WriteError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	tx, err := s.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var code string
	var bookingName string
	var bookingID int64
	err = tx.QueryRow(ctx, `
SELECT t.code, b.name, b.id
FROM tickets t
JOIN bookings b ON b.id = t.booking_id
WHERE t.id=$1`, ticketID).Scan(&code, &bookingName, &bookingID)
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, "A ticket with this ID does not exist")
		return
	}
	if code != req.Code || bookingName != req.Name {
		httpx.WriteError(w, http.StatusUnauthorized, "Unauthorized")
		return
	}

	if _, err := tx.Exec(ctx, `UPDATE location_seats SET ticket_id=NULL WHERE ticket_id=$1`, ticketID); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := tx.Exec(ctx, `DELETE FROM tickets WHERE id=$1`, ticketID); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	if err := tx.Commit(ctx); err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// 204 No Content
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) fetchTicketsByBooking(ctx context.Context, bookingID int64) ([]ticketDTO, error) {
	rows, err := s.DB.Query(ctx, `
SELECT
  t.id,
  t.code,
  b.name,
  t.created_at,
  r.id,
  r.name,
  s.number,
  sh.id,
  sh.start,
  sh."end",
  c.id,
  c.artist,
  l.id,
  l.name
FROM tickets t
JOIN bookings b ON b.id = t.booking_id
JOIN location_seats s ON s.ticket_id = t.id
JOIN location_seat_rows r ON r.id = s.location_seat_row_id
JOIN shows sh ON sh.id = r.show_id
JOIN concerts c ON c.id = sh.concert_id
JOIN locations l ON l.id = c.location_id
WHERE t.booking_id = $1
ORDER BY t.id`, bookingID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]ticketDTO, 0)
	for rows.Next() {
		var (
			td                 ticketDTO
			createdAt          time.Time
			showStart, showEnd time.Time
		)
		if err := rows.Scan(
			&td.ID,
			&td.Code,
			&td.Name,
			&createdAt,
			&td.Row.ID,
			&td.Row.Name,
			&td.Seat,
			&td.Show.ID,
			&showStart,
			&showEnd,
			&td.Show.Concert.ID,
			&td.Show.Concert.Artist,
			&td.Show.Concert.Location.ID,
			&td.Show.Concert.Location.Name,
		); err != nil {
			return nil, err
		}
		td.CreatedAt = createdAt.UTC().Format(time.RFC3339)
		td.Show.Start = showStart.UTC().Format(time.RFC3339)
		td.Show.End = showEnd.UTC().Format(time.RFC3339)
		out = append(out, td)
	}
	return out, rows.Err()
}

func newTicketCode() string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 10)
	if _, err := rand.Read(b); err == nil {
		for i := range b {
			b[i] = alphabet[int(b[i])%len(alphabet)]
		}
		return string(b)
	}
	// fallback
	return fmt.Sprintf("T%09d", time.Now().UnixNano()%1_000_000_000)
}
