ALTER TABLE tickets DROP CONSTRAINT IF EXISTS tickets_code_unique;
ALTER TABLE reservations DROP CONSTRAINT IF EXISTS reservations_token_unique;

