-- YesLogs CRM/RADIUS reference schema helpers.
--
-- REVIEW AND ADAPT these statements to the CRM's real schema before running.
-- The PHP endpoint never modifies radacct or customer data.

-- 1. Standardize the CRM customer table behind this five-column view.
--    Change customers/id/name/address/phone below to the CRM's actual names.
CREATE OR REPLACE VIEW yeslogs_subscribers AS
SELECT
    CAST(c.id AS CHAR) AS account_id,
    c.username         AS username,
    c.name             AS name,
    c.address          AS address,
    CAST(c.phone AS CHAR) AS phone
FROM customers c;

-- The subscriber username must resolve to no more than one customer row.
-- Add/confirm an equivalent unique index on the real customer table:
-- CREATE UNIQUE INDEX uq_customers_username ON customers (username);

-- 2. Required performance index for IP + event-time matching.
--    Check existing radacct indexes first and do not create a duplicate.
CREATE INDEX idx_yeslogs_radacct_ip_time
    ON radacct (framedipaddress, acctstarttime, acctstoptime);

-- 3. If CRM_NAS_MATCH_MODE is "required", this alternative index is usually
--    better. Use it instead of (not necessarily in addition to) the index above.
-- CREATE INDEX idx_yeslogs_radacct_ip_nas_time
--     ON radacct (framedipaddress, nasipaddress, acctstarttime, acctstoptime);

-- Minimum DB grants for the endpoint user (adjust database name/host):
-- GRANT SELECT, CREATE TEMPORARY TABLES ON radius.* TO
--     'yeslogs_reader'@'127.0.0.1' IDENTIFIED BY '<strong-random-password>';
