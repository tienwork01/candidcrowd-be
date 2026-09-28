DROP INDEX events_host_client_request_id_idx;
ALTER TABLE events DROP COLUMN client_request_id;
