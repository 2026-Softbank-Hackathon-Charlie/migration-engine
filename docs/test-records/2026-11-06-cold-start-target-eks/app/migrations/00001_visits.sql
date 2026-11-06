-- +goose Up
CREATE TABLE visits (id bigint PRIMARY KEY, at timestamptz NOT NULL, via text NOT NULL);
-- +goose Down
DROP TABLE visits;
