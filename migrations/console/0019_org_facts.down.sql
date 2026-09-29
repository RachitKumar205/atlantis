-- The facts are a copy of what each organisation's own database holds, so
-- dropping them loses the fleet view until the next sweep on the way back up,
-- and nothing else.
DROP TABLE IF EXISTS console.org_facts;
