-- Dropping caller_certs loses the fingerprint→organisation mapping renewal
-- depends on, so every machine holding a certificate issued through enrolment
-- has to enrol again. The certificates keep working — atlantis authenticates
-- them from its own caller_identities row — they simply cannot be renewed.
DROP TABLE IF EXISTS console.caller_certs;
DROP TABLE IF EXISTS console.enroll_tokens;
