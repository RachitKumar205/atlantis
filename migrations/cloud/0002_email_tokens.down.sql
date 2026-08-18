-- Reverses 0002. Any verification or reset link already sent stops working,
-- which is the correct outcome — the rows that would have honoured them are
-- gone, and a token nothing can check must not be accepted.
DROP TABLE IF EXISTS cloud.email_tokens;
