package api

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"time"

	"concerts-go/internal/httpx"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Server struct {
	DB *pgxpool.Pool
}

func Routes(db *pgxpool.Pool) http.Handler {
	s := &Server{DB: db}
	r := chi.NewRouter()

	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/concerts", s.getConcerts)
		r.Get("/concerts/{concert-id}", s.getConcert)
		r.Get("/concerts/{concert-id}/shows/{show-id}/seating", s.getSeating)
	})

	return r
}

func (s *Server) getConcerts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Fetch concerts + locations
	rows, err := s.DB.Query(ctx, `
SELECT c.id, c.artist, l.id, l.name
FROM concerts c
JOIN locations l ON l.id = c.location_id
ORDER BY c.id`)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer rows.Close()

	concerts := make([]concertDTO, 0)
	index := make(map[int64]int)

	for rows.Next() {
		var c concertDTO
		if err := rows.Scan(&c.ID, &c.Artist, &c.Location.ID, &c.Location.Name); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "internal error")
			return
		}
		c.Shows = []showDTO{}
		index[c.ID] = len(concerts)
		concerts = append(concerts, c)
	}
	if rows.Err() != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Fetch shows for all concerts
	showRows, err := s.DB.Query(ctx, `
SELECT id, concert_id, start, "end"
FROM shows
ORDER BY id`)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer showRows.Close()

	for showRows.Next() {
		var (
			id        int64
			concertID int64
			start     time.Time
			end       time.Time
		)
		if err := showRows.Scan(&id, &concertID, &start, &end); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "internal error")
			return
		}

		pos, ok := index[concertID]
		if !ok {
			continue
		}
		concerts[pos].Shows = append(concerts[pos].Shows, showDTO{
			ID:    id,
			Start: start.UTC().Format(time.RFC3339),
			End:   end.UTC().Format(time.RFC3339),
		})
	}
	if showRows.Err() != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{"concerts": concerts})
}

func (s *Server) getConcert(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	concertID, ok := parseID(chi.URLParam(r, "concert-id"))
	if !ok {
		httpx.WriteError(w, http.StatusNotFound, "A concert with this ID does not exist")
		return
	}

	var c concertDTO
	err := s.DB.QueryRow(ctx, `
SELECT c.id, c.artist, l.id, l.name
FROM concerts c
JOIN locations l ON l.id = c.location_id
WHERE c.id = $1`, concertID).Scan(&c.ID, &c.Artist, &c.Location.ID, &c.Location.Name)
	if err != nil {
		httpx.WriteError(w, http.StatusNotFound, "A concert with this ID does not exist")
		return
	}

	c.Shows = []showDTO{}
	showRows, err := s.DB.Query(ctx, `
SELECT id, start, "end"
FROM shows
WHERE concert_id = $1
ORDER BY id`, concertID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer showRows.Close()

	for showRows.Next() {
		var (
			id    int64
			start time.Time
			end   time.Time
		)
		if err := showRows.Scan(&id, &start, &end); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "internal error")
			return
		}
		c.Shows = append(c.Shows, showDTO{
			ID:    id,
			Start: start.UTC().Format(time.RFC3339),
			End:   end.UTC().Format(time.RFC3339),
		})
	}
	if showRows.Err() != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{"concert": c})
}

func (s *Server) getSeating(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	concertID, ok1 := parseID(chi.URLParam(r, "concert-id"))
	showID, ok2 := parseID(chi.URLParam(r, "show-id"))
	if !ok1 || !ok2 {
		httpx.WriteError(w, http.StatusNotFound, "A concert or show with this ID does not exist")
		return
	}

	// Validate show belongs to concert
	var exists bool
	if err := s.DB.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM shows WHERE id=$1 AND concert_id=$2)`, showID, concertID).Scan(&exists); err != nil || !exists {
		httpx.WriteError(w, http.StatusNotFound, "A concert or show with this ID does not exist")
		return
	}

	type rowRec struct {
		id    int64
		name  string
		order int
		total int
	}
	rowRows, err := s.DB.Query(ctx, `
SELECT r.id, r.name, r."order", COUNT(s.id) AS total
FROM location_seat_rows r
JOIN location_seats s ON s.location_seat_row_id = r.id
WHERE r.show_id = $1
GROUP BY r.id, r.name, r."order"
ORDER BY r."order"`, showID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer rowRows.Close()

	rowsOut := make([]seatingRowDTO, 0)
	for rowRows.Next() {
		var rr rowRec
		if err := rowRows.Scan(&rr.id, &rr.name, &rr.order, &rr.total); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "internal error")
			return
		}

		out := seatingRowDTO{ID: rr.id, Name: rr.name}
		out.Seats.Total = rr.total
		out.Seats.Unavailable = []int{}
		rowsOut = append(rowsOut, out)
	}
	if rowRows.Err() != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Unavailable seats per row (reserved or ticketed)
	unRows, err := s.DB.Query(ctx, `
SELECT location_seat_row_id, number
FROM location_seats
WHERE location_seat_row_id = ANY (
  SELECT id FROM location_seat_rows WHERE show_id = $1
)
AND (reservation_id IS NOT NULL OR ticket_id IS NOT NULL)
ORDER BY location_seat_row_id, number`, showID)
	if err != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer unRows.Close()

	byRow := make(map[int64][]int)
	for unRows.Next() {
		var rowID int64
		var num int
		if err := unRows.Scan(&rowID, &num); err != nil {
			httpx.WriteError(w, http.StatusInternalServerError, "internal error")
			return
		}
		byRow[rowID] = append(byRow[rowID], num)
	}
	if unRows.Err() != nil {
		httpx.WriteError(w, http.StatusInternalServerError, "internal error")
		return
	}

	for i := range rowsOut {
		un := byRow[rowsOut[i].ID]
		if un == nil {
			un = []int{}
		}
		sort.Ints(un)
		rowsOut[i].Seats.Unavailable = un
	}

	httpx.WriteJSON(w, http.StatusOK, map[string]any{"rows": rowsOut})
}

func parseID(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v <= 0 {
		return 0, false
	}
	return v, true
}

// ensure unused import does not happen when context later used
var _ = context.Background

