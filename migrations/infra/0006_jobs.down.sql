DROP TRIGGER IF EXISTS jobs_notify ON atlantis.jobs;
DROP FUNCTION IF EXISTS atlantis.notify_job_enqueue();
DROP TABLE IF EXISTS atlantis.job_schedules;
DROP TABLE IF EXISTS atlantis.jobs_dead;
DROP TABLE IF EXISTS atlantis.jobs;
