-- add constraints helpful for API correctness

ALTER TABLE reservations
  ADD CONSTRAINT reservations_token_unique UNIQUE (token);

ALTER TABLE tickets
  ADD CONSTRAINT tickets_code_unique UNIQUE (code);

