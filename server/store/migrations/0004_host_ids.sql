-- People, teams and credentials belong to the host, so targeting and report
-- rows hold the host's opaque ids as text and the server's own directory
-- tables go. Stored uuids carry over as their text form.
ALTER TABLE enforced_artifacts
    DROP CONSTRAINT enforced_artifacts_person_id_fkey,
    DROP CONSTRAINT enforced_artifacts_team_id_fkey;
ALTER TABLE enforced_artifacts
    ALTER COLUMN person_id TYPE text USING person_id::text,
    ALTER COLUMN team_id TYPE text USING team_id::text,
    ADD CONSTRAINT enforced_artifacts_ids_chk CHECK (person_id <> '' AND team_id <> '');

ALTER TABLE report_installed DROP CONSTRAINT report_installed_person_id_fkey;
ALTER TABLE report_apply_failures DROP CONSTRAINT report_apply_failures_person_id_fkey;
ALTER TABLE agent_reports DROP CONSTRAINT agent_reports_person_id_fkey;
ALTER TABLE agent_reports
    ALTER COLUMN person_id TYPE text USING person_id::text,
    ADD CONSTRAINT agent_reports_person_id_chk CHECK (person_id <> '');
ALTER TABLE report_installed ALTER COLUMN person_id TYPE text USING person_id::text;
ALTER TABLE report_apply_failures ALTER COLUMN person_id TYPE text USING person_id::text;
ALTER TABLE report_installed ADD CONSTRAINT report_installed_person_id_fkey
    FOREIGN KEY (person_id) REFERENCES agent_reports (person_id) ON DELETE CASCADE;
ALTER TABLE report_apply_failures ADD CONSTRAINT report_apply_failures_person_id_fkey
    FOREIGN KEY (person_id) REFERENCES agent_reports (person_id) ON DELETE CASCADE;

DROP TABLE device_tokens, team_members, teams, persons;
