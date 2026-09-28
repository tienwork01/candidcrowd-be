ALTER TABLE events ADD COLUMN client_request_id uuid;
CREATE UNIQUE INDEX events_host_client_request_id_idx ON events (host_id, client_request_id) WHERE client_request_id IS NOT NULL;
