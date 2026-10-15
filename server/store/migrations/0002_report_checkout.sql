-- Two checkouts of one repository report the same path, so the checkout key
-- joins the report identity. '' is user-global or a pre-checkout report.
ALTER TABLE report_installed ADD COLUMN checkout text NOT NULL DEFAULT '';
ALTER TABLE report_installed DROP CONSTRAINT report_installed_pkey;
ALTER TABLE report_installed ADD PRIMARY KEY (person_id, ide_type, repository, checkout, source_path);
