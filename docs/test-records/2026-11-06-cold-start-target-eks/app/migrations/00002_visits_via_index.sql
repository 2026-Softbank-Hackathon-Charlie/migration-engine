-- +goose Up
CREATE INDEX visits_via ON visits (via);
-- +goose Down
DROP INDEX visits_via;
