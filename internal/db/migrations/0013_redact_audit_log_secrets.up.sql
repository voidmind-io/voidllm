-- Migration: 0013_redact_audit_log_secrets.up.sql
-- Description: One-time cleanup of audit_logs rows that were written before the
-- redaction fix landed. Prior to the fix, the audit middleware serialised the
-- raw Admin API request body as the description, which could contain plaintext
-- secrets for any field that is now listed in sensitiveFields:
--
--   password, api_key, auth_token, oauth_client_secret, client_secret, key
--
-- The description column is NOT NULL (empty string = no description), so
-- affected rows are cleared to '' rather than NULL.
--
-- IMPORTANT: Any secrets that may have been stored in audit_logs.description
-- should be considered compromised. Rotate all credentials that were configured
-- via the Admin API before this fix was applied (API keys, MCP auth tokens,
-- SSO client secrets, model API keys, user passwords, license keys).
--
-- Pattern strategy: each LIKE pattern matches the JSON key as it would appear
-- in a serialised object ('"key_name"'). A JSON key is always preceded by either
-- '{' (opening brace) or ',' (field separator), and followed by '":'. Matching
-- '"key_name":' is sufficient and portable across both SQLite and PostgreSQL.
--
-- Case-sensitivity note: SQLite LIKE is case-insensitive for ASCII letters by
-- default; PostgreSQL LIKE is case-sensitive. The patterns below use lowercase
-- only, which covers the standard API usage where field names are always
-- lowercase. Mixed-case or uppercase variants of these field names are not
-- matched by this migration. If variant casing is a concern, run a manual
-- query using ILIKE (PostgreSQL) or LOWER(description) LIKE (SQLite) after
-- this migration completes.

UPDATE audit_logs
SET    description = ''
WHERE  description != ''
  AND (
        description LIKE '%"password":%'
     OR description LIKE '%"api_key":%'
     OR description LIKE '%"auth_token":%'
     OR description LIKE '%"oauth_client_secret":%'
     OR description LIKE '%"client_secret":%'
     OR description LIKE '%"key":%'
     OR description LIKE '%"token":%'
     OR description LIKE '%"license":%'
  );
