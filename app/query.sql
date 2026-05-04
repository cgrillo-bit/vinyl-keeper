-- name: CreateUser :one
INSERT INTO users(user_name) VALUES (?)
RETURNING *;

-- name: ListUsers :many
SELECT * FROM users ORDER BY user_name ASC;

-- name: GetUserByID :one
SELECT * FROM users WHERE user_id = ?;

-- name: RegisterVinylUnique :one
INSERT INTO vinyl_unique(
    vinyl_title,
    vinyl_artist,
    master_id,
    styles,
    genres
) VALUES (?, ?, ?, ?, ?)
ON CONFLICT
DO UPDATE SET vinyl_title = vinyl_title
RETURNING *;

-- name: UpsertVinylRelease :one
INSERT INTO vinyl_release(
    vinyl_id,
    release_id,
    lowest_price,
    price_last_updated,
    country,
    notes,
    released,
    master_release,
    resource_uri,
    image_extension,
    cover_raw_blob,
    cover_embedding
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(vinyl_id, release_id)
DO UPDATE SET
    lowest_price = excluded.lowest_price,
    price_last_updated = excluded.price_last_updated,
    country = excluded.country,
    notes = excluded.notes,
    released = excluded.released,
    master_release = excluded.master_release,
    resource_uri = excluded.resource_uri,
    image_extension = excluded.image_extension,
    cover_raw_blob = excluded.cover_raw_blob,
    cover_embedding = excluded.cover_embedding
RETURNING *;

-- name: GetAllVinylRecords :many
SELECT
    vu.vinyl_id,
    vu.vinyl_title,
    vu.vinyl_artist,
    vu.master_id,
    vu.styles,
    vu.genres,
    CAST(substr(vr.released, 1, 4) AS INTEGER) AS vinyl_pressing_year,
    vr.country,
    vr.lowest_price AS recent_price,
    vr.released,
    vr.image_extension,
    vr.cover_raw_blob,
    vr.cover_embedding
FROM vinyl_unique vu
JOIN vinyl_release vr
    ON vr.vinyl_id = vu.vinyl_id
   AND vr.master_release = 1
ORDER BY vu.vinyl_artist ASC, vu.vinyl_title ASC;

-- name: GetVinylRecordByID :one
SELECT
    vu.vinyl_id,
    vu.vinyl_title,
    vu.vinyl_artist,
    vu.master_id,
    vu.styles,
    vu.genres,
    CAST(substr(vr.released, 1, 4) AS INTEGER) AS vinyl_pressing_year,
    vr.country,
    vr.lowest_price AS recent_price,
    vr.released,
    vr.image_extension,
    vr.cover_raw_blob,
    vr.cover_embedding
FROM vinyl_unique vu
JOIN vinyl_release vr
    ON vr.vinyl_id = vu.vinyl_id
   AND vr.master_release = 1
WHERE vu.vinyl_id = ?;

-- name: GetPrimaryReleaseID :one
SELECT release_id
FROM vinyl_release
WHERE vinyl_id = ? AND master_release = 1
LIMIT 1;

-- name: EnsureOwnershipPlay :exec
INSERT OR IGNORE INTO vinyl_plays(user_id, vinyl_id, release_id, play, played_date)
VALUES (?, ?, ?, 0, ?);

-- name: NextPlayNumber :one
SELECT COALESCE(MAX(play), 0) + 1
FROM vinyl_plays
WHERE user_id = ?
  AND vinyl_id = ?
  AND release_id = ?;

-- name: InsertVinylPlay :exec
INSERT INTO vinyl_plays(user_id, vinyl_id, release_id, play, played_date)
VALUES (?, ?, ?, ?, ?);

-- name: GetUserVinylPlays :many
SELECT
    user_id,
    vinyl_id,
    CAST(SUM(CASE WHEN play >= 1 THEN 1 ELSE 0 END) AS INTEGER) AS plays,
    CAST(MIN(played_date) AS TEXT) AS first_played,
    CAST(MAX(played_date) AS TEXT) AS last_played
FROM vinyl_plays
WHERE user_id = ?
GROUP BY user_id, vinyl_id
ORDER BY last_played DESC;

-- name: GetAllUserVinylPlays :many
SELECT
    user_id,
    vinyl_id,
    CAST(SUM(CASE WHEN play >= 1 THEN 1 ELSE 0 END) AS INTEGER) AS plays,
    CAST(MIN(played_date) AS TEXT) AS first_played,
    CAST(MAX(played_date) AS TEXT) AS last_played
FROM vinyl_plays
GROUP BY user_id, vinyl_id;

-- name: DeleteVinyl :exec
DELETE FROM vinyl_unique WHERE vinyl_id = ?;

-- name: DeleteUser :exec
DELETE FROM users WHERE user_id = ?;
