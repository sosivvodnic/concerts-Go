-- Fix identity sequences after seeding explicit IDs.
-- Without this, subsequent INSERTs can fail with duplicate key violations.

SELECT setval(pg_get_serial_sequence('locations', 'id'), COALESCE((SELECT MAX(id) FROM locations), 1), true);
SELECT setval(pg_get_serial_sequence('concerts', 'id'), COALESCE((SELECT MAX(id) FROM concerts), 1), true);
SELECT setval(pg_get_serial_sequence('shows', 'id'), COALESCE((SELECT MAX(id) FROM shows), 1), true);
SELECT setval(pg_get_serial_sequence('bookings', 'id'), COALESCE((SELECT MAX(id) FROM bookings), 1), true);
SELECT setval(pg_get_serial_sequence('reservations', 'id'), COALESCE((SELECT MAX(id) FROM reservations), 1), true);
SELECT setval(pg_get_serial_sequence('tickets', 'id'), COALESCE((SELECT MAX(id) FROM tickets), 1), true);
SELECT setval(pg_get_serial_sequence('location_seat_rows', 'id'), COALESCE((SELECT MAX(id) FROM location_seat_rows), 1), true);
SELECT setval(pg_get_serial_sequence('location_seats', 'id'), COALESCE((SELECT MAX(id) FROM location_seats), 1), true);

